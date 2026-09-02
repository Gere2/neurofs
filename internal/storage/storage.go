// Package storage manages NeuroFS's SQLite index.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/models"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    path       TEXT    NOT NULL UNIQUE,
	    rel_path   TEXT    NOT NULL,
	    lang       TEXT    NOT NULL,
	    size       INTEGER NOT NULL,
	    mtime_ns   INTEGER NOT NULL DEFAULT 0,
	    lines      INTEGER NOT NULL,
    symbols    TEXT    NOT NULL DEFAULT '[]',
    imports    TEXT    NOT NULL DEFAULT '[]',
    checksum   TEXT    NOT NULL,
    indexed_at TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_files_rel_path ON files(rel_path);
CREATE INDEX IF NOT EXISTS idx_files_lang     ON files(lang);

CREATE TABLE IF NOT EXISTS metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp     TEXT    NOT NULL,
    model         TEXT    NOT NULL,
    query         TEXT    NOT NULL,
    tokens_before INTEGER NOT NULL,
    tokens_after  INTEGER NOT NULL,
    saved_tokens  INTEGER NOT NULL,
    savings_usd   REAL    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_proxy_logs_timestamp ON proxy_logs(timestamp);

	CREATE TABLE IF NOT EXISTS file_embeddings (
	    path      TEXT PRIMARY KEY,
	    embedding BLOB NOT NULL,
	    checksum  TEXT NOT NULL DEFAULT '',
	    provider  TEXT NOT NULL DEFAULT '',
	    model     TEXT NOT NULL DEFAULT '',
	    FOREIGN KEY(path) REFERENCES files(path) ON DELETE CASCADE
	);

CREATE TABLE IF NOT EXISTS file_relations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    source_path TEXT    NOT NULL,
    target_path TEXT    NOT NULL,
    rel_type    TEXT    NOT NULL,
    FOREIGN KEY(source_path) REFERENCES files(path) ON DELETE CASCADE,
    UNIQUE(source_path, target_path, rel_type)
);

CREATE INDEX IF NOT EXISTS idx_relations_source ON file_relations(source_path);
CREATE INDEX IF NOT EXISTS idx_relations_target ON file_relations(target_path);

CREATE TABLE IF NOT EXISTS chunks (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    file_path      TEXT    NOT NULL,
    chunk_id       TEXT    NOT NULL,
    parent_id      TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL,
    symbol         TEXT    NOT NULL DEFAULT '',
    start_line     INTEGER NOT NULL,
    end_line       INTEGER NOT NULL,
    content_hash   TEXT    NOT NULL,
    ast_hash       TEXT    NOT NULL DEFAULT '',
    heading_path   TEXT    NOT NULL DEFAULT '',
    calls          TEXT    NOT NULL DEFAULT '[]',
    token_estimate INTEGER NOT NULL DEFAULT 0,
    indexed_at     TEXT    NOT NULL,
    UNIQUE(file_path, chunk_id),
    FOREIGN KEY(file_path) REFERENCES files(path) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_chunks_file_path ON chunks(file_path);
CREATE INDEX IF NOT EXISTS idx_chunks_content_hash ON chunks(content_hash);

CREATE TABLE IF NOT EXISTS chunk_embeddings (
    content_hash TEXT PRIMARY KEY,
    embedding    BLOB NOT NULL,
    provider     TEXT NOT NULL,
    model        TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
`

// DB wraps a SQLite connection and provides typed read/write operations.
type DB struct {
	db   *sql.DB
	path string
	// hasHeadingPath records whether chunks.heading_path exists in this
	// index. Read-only opens (gate, cross-shape verification, any
	// --disable-index-refresh measurement) deliberately skip migrations, so
	// they can be handed an index written by an older binary; chunk reads
	// degrade to an empty heading path there instead of failing.
	hasHeadingPath bool
}

// ChunkSearchOptions filters chunk lookups.
type ChunkSearchOptions struct {
	FilePath    string
	Symbol      string
	ContentHash string
	Limit       int
}

// Open opens (or creates) the NeuroFS index database at the given path.
func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("storage: create db dir: %w", err)
	}
	expectedInfo, err := prepareDatabasePath(dbPath)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("storage: open sqlite: %w", err)
	}

	// Keep every connection-scoped PRAGMA on the one connection this handle
	// will use.
	db.SetMaxOpenConns(1) // SQLite is single-writer

	// Force SQLite to open the file now, then ensure the path still names the
	// regular file inspected (or securely created) above. This rejects the
	// stable index.db -> external-file symlink case before any schema writes
	// and narrows replacement races around sql.Open.
	if err := execWithBusyRetry(db, "PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: connect sqlite: %w", err)
	}
	if err := verifyDatabasePath(dbPath, expectedInfo); err != nil {
		_ = db.Close()
		return nil, err
	}

	// busy_timeout must be set before journal_mode so the WAL switch
	// itself waits when another process is mid-switch. WAL lets readers
	// proceed during writes; synchronous=NORMAL is the documented safe
	// pair for WAL. Without these, two concurrent `neurofs scan`
	// invocations collide instantly with SQLITE_BUSY.
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if err := execWithBusyRetry(db, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storage: %s: %w", p, err)
		}
	}

	if err := execWithBusyRetry(db, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: apply schema: %w", err)
	}
	if err := ensureColumn(db, "chunks", "calls", `ALTER TABLE chunks ADD COLUMN calls TEXT NOT NULL DEFAULT '[]'`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate chunks.calls: %w", err)
	}
	migrations := []struct {
		table  string
		column string
		ddl    string
	}{
		{"chunks", "heading_path", `ALTER TABLE chunks ADD COLUMN heading_path TEXT NOT NULL DEFAULT ''`},
		{"files", "mtime_ns", `ALTER TABLE files ADD COLUMN mtime_ns INTEGER NOT NULL DEFAULT 0`},
		{"file_embeddings", "checksum", `ALTER TABLE file_embeddings ADD COLUMN checksum TEXT NOT NULL DEFAULT ''`},
		{"file_embeddings", "provider", `ALTER TABLE file_embeddings ADD COLUMN provider TEXT NOT NULL DEFAULT ''`},
		{"file_embeddings", "model", `ALTER TABLE file_embeddings ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
	}
	for _, migration := range migrations {
		if err := ensureColumn(db, migration.table, migration.column, migration.ddl); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storage: migrate %s.%s: %w", migration.table, migration.column, err)
		}
	}

	return &DB{db: db, path: dbPath, hasHeadingPath: hasColumn(db, "chunks", "heading_path")}, nil
}

// OpenReadOnly opens an existing NeuroFS index without creating directories,
// applying schema migrations, or changing SQLite journal settings. It is for
// measurement paths such as gate that must observe the exact indexed
// generation already on disk.
func OpenReadOnly(dbPath string) (*DB, error) {
	expectedInfo, err := inspectDatabasePath(dbPath)
	if err != nil {
		return nil, err
	}
	if err := rejectPendingWAL(dbPath); err != nil {
		return nil, err
	}

	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	query := dsn.Query()
	// immutable prevents SQLite from creating otherwise-empty -wal/-shm
	// coordination files for a read. A non-empty WAL is rejected above rather
	// than silently ignored, so the immutable snapshot cannot omit commits.
	query.Set("immutable", "1")
	query.Set("mode", "ro")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("storage: open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)

	// Both pragmas are connection-local. busy_timeout gives concurrent scans a
	// chance to finish; query_only is defense in depth in addition to mode=ro.
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA query_only = ON",
	} {
		if err := execWithBusyRetry(db, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storage: %s: %w", pragma, err)
		}
	}
	if err := verifyDatabasePath(dbPath, expectedInfo); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := rejectPendingWAL(dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db, path: dbPath, hasHeadingPath: hasColumn(db, "chunks", "heading_path")}, nil
}

func prepareDatabasePath(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil && !os.IsExist(createErr) {
			return nil, fmt.Errorf("storage: create database file: %w", createErr)
		}
		if createErr == nil {
			if closeErr := file.Close(); closeErr != nil {
				return nil, fmt.Errorf("storage: close new database file: %w", closeErr)
			}
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: inspect database file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("storage: database path must be a regular non-symlink file: %s", path)
	}
	return info, nil
}

func inspectDatabasePath(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("storage: inspect database file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("storage: database path must be a regular non-symlink file: %s", path)
	}
	return info, nil
}

func rejectPendingWAL(dbPath string) error {
	info, err := os.Stat(dbPath + "-wal")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: inspect read-only WAL: %w", err)
	}
	if info.Size() > 0 {
		return fmt.Errorf("storage: cannot take immutable read-only snapshot with a non-empty WAL: %s-wal", dbPath)
	}
	return nil
}

func verifyDatabasePath(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("storage: verify database file: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
		return fmt.Errorf("storage: database path must be a regular non-symlink file: %s", path)
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("storage: database path changed while opening: %s", path)
	}
	return nil
}

// hasColumn reports whether table already has column. Errors are reported as
// "absent" on purpose: the callers use this to pick a tolerant query shape,
// and a PRAGMA failure must not take a read-only measurement down.
func hasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer closeRows(rows)
	for rows.Next() {
		var (
			cid        int
			name       string
			typ        string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer closeRows(rows)
	for rows.Next() {
		var (
			cid        int
			name       string
			typ        string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// MaxOpenConns(1) keeps connection-scoped PRAGMAs reliable. Release that
	// sole connection before executing a missing-column migration.
	if err := rows.Close(); err != nil {
		return err
	}
	return execWithBusyRetry(db, ddl)
}

func execWithBusyRetry(db *sql.DB, stmt string) error {
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		if _, err := db.Exec(stmt); err != nil {
			if !isSQLiteBusy(err) {
				return err
			}
			last = err
			time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
			continue
		}
		return nil
	}
	return last
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "sqlite_busy") ||
		strings.Contains(text, "database is locked") ||
		strings.Contains(text, "database table is locked")
}

func closeRows(rows *sql.Rows) {
	_ = rows.Close()
}

func closeStmt(stmt *sql.Stmt) {
	_ = stmt.Close()
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

// Close closes the underlying database connection.
func (s *DB) Close() error {
	return s.db.Close()
}

// Path returns the file-system path of the database.
func (s *DB) Path() string {
	return s.path
}

// UpsertFile inserts or replaces a FileRecord.
func (s *DB) UpsertFile(f models.FileRecord) error {
	syms, err := json.Marshal(f.Symbols)
	if err != nil {
		return fmt.Errorf("storage: marshal symbols: %w", err)
	}
	imps, err := json.Marshal(f.Imports)
	if err != nil {
		return fmt.Errorf("storage: marshal imports: %w", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO files (path, rel_path, lang, size, mtime_ns, lines, symbols, imports, checksum, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			rel_path   = excluded.rel_path,
			lang       = excluded.lang,
			size       = excluded.size,
			mtime_ns   = excluded.mtime_ns,
			lines      = excluded.lines,
			symbols    = excluded.symbols,
			imports    = excluded.imports,
			checksum   = excluded.checksum,
			indexed_at = excluded.indexed_at
		`,
		f.Path, f.RelPath, string(f.Lang),
		f.Size, f.ModTimeUnixNano, f.Lines,
		string(syms), string(imps),
		f.Checksum, f.IndexedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("storage: upsert %s: %w", f.RelPath, err)
	}
	return nil
}

// UpsertFileAndChunks atomically publishes a file record and its chunk
// generation. If the source checksum changed, the previous file-level
// embedding is invalidated in the same transaction so readers can never pair a
// new FileRecord with a stale vector.
func (s *DB) UpsertFileAndChunks(f models.FileRecord, chunks []models.Chunk) error {
	syms, err := json.Marshal(f.Symbols)
	if err != nil {
		return fmt.Errorf("storage: marshal symbols: %w", err)
	}
	imps, err := json.Marshal(f.Imports)
	if err != nil {
		return fmt.Errorf("storage: marshal imports: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin file generation: %w", err)
	}
	defer rollback(tx)

	var previousChecksum string
	err = tx.QueryRow(`SELECT checksum FROM files WHERE path = ?`, f.Path).Scan(&previousChecksum)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("storage: read previous checksum: %w", err)
	}
	previousExists := err == nil

	if _, err := tx.Exec(`
		INSERT INTO files (path, rel_path, lang, size, mtime_ns, lines, symbols, imports, checksum, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			rel_path = excluded.rel_path,
			lang = excluded.lang,
			size = excluded.size,
			mtime_ns = excluded.mtime_ns,
			lines = excluded.lines,
			symbols = excluded.symbols,
			imports = excluded.imports,
			checksum = excluded.checksum,
			indexed_at = excluded.indexed_at
	`,
		f.Path, f.RelPath, string(f.Lang), f.Size, f.ModTimeUnixNano,
		f.Lines, string(syms), string(imps), f.Checksum,
		f.IndexedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("storage: upsert file generation %s: %w", f.RelPath, err)
	}

	if err := replaceChunksInTx(tx, f.Path, chunks); err != nil {
		return err
	}
	if previousExists && previousChecksum != f.Checksum {
		if _, err := tx.Exec(`DELETE FROM file_embeddings WHERE path = ?`, f.Path); err != nil {
			return fmt.Errorf("storage: invalidate stale file embedding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit file generation: %w", err)
	}
	return nil
}

// AllFiles returns every FileRecord in the index.
func (s *DB) AllFiles() ([]models.FileRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, path, rel_path, lang, size, mtime_ns, lines, symbols, imports, checksum, indexed_at
		FROM files
		ORDER BY rel_path
	`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var records []models.FileRecord
	for rows.Next() {
		r, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// GetFileByRelPath returns a FileRecord by its relative path.
func (s *DB) GetFileByRelPath(relPath string) (models.FileRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, path, rel_path, lang, size, mtime_ns, lines, symbols, imports, checksum, indexed_at
		FROM files
		WHERE rel_path = ?
	`, relPath)
	if err != nil {
		return models.FileRecord{}, err
	}
	defer closeRows(rows)

	if !rows.Next() {
		return models.FileRecord{}, fmt.Errorf("file not found: %s", relPath)
	}

	return scanFile(rows)
}

// FileCount returns the total number of indexed files.
func (s *DB) FileCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	return n, err
}

// LangBreakdown returns the count of indexed files grouped by language.
func (s *DB) LangBreakdown() (map[models.Lang]int, error) {
	rows, err := s.db.Query(`SELECT lang, COUNT(*) FROM files GROUP BY lang ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	out := make(map[models.Lang]int)
	for rows.Next() {
		var (
			lang string
			n    int
		)
		if err := rows.Scan(&lang, &n); err != nil {
			return nil, err
		}
		out[models.Lang(lang)] = n
	}
	return out, rows.Err()
}

// LastIndexedAt returns the most recent indexed_at timestamp across all files,
// or the zero time when the index is empty.
func (s *DB) LastIndexedAt() (time.Time, error) {
	var raw string
	err := s.db.QueryRow(`SELECT COALESCE(MAX(indexed_at), '') FROM files`).Scan(&raw)
	if err != nil {
		return time.Time{}, err
	}
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// TotalBytes returns the cumulative byte size of all indexed files.
func (s *DB) TotalBytes() (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM files`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// DBSize returns the size of the SQLite database file in bytes.
func (s *DB) DBSize() (int64, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// SetMeta stores a key-value pair in the metadata table.
func (s *DB) SetMeta(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

// GetMeta retrieves a value by key; returns ("", false, nil) when not found.
func (s *DB) GetMeta(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM metadata WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// DeleteRemovedFiles deletes records whose paths are no longer present on
// disk and returns the number of records deleted. The deletes run inside a
// single transaction so an error partway through rolls back to the original
// state — otherwise the caller's reported "Removed" count and the actual
// on-disk index would diverge on failure.
func (s *DB) DeleteRemovedFiles(existingPaths map[string]bool) (int, error) {
	rows, err := s.db.Query(`SELECT path FROM files`)
	if err != nil {
		return 0, err
	}
	defer closeRows(rows)

	var toDelete []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		if !existingPaths[p] {
			toDelete = append(toDelete, p)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// rows must be closed before opening a write transaction on the same
	// single-connection SQLite handle; otherwise the tx would deadlock
	// waiting for the cursor.
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("storage: close path cursor: %w", err)
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("storage: begin tx: %w", err)
	}
	defer rollback(tx)

	for _, p := range toDelete {
		if err := deleteFileRecord(tx, p); err != nil {
			return 0, fmt.Errorf("storage: delete %s: %w", p, err)
		}
	}
	if err := pruneUnreferencedChunkEmbeddings(tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("storage: commit delete: %w", err)
	}
	return len(toDelete), nil
}

// DeleteFile deletes a single file record by path.
func (s *DB) DeleteFile(path string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin delete file: %w", err)
	}
	defer rollback(tx)

	if err := deleteFileRecord(tx, path); err != nil {
		return err
	}
	if err := pruneUnreferencedChunkEmbeddings(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit delete file: %w", err)
	}
	return nil
}

// DeletePathTree deletes an indexed file or every indexed descendant of a
// directory path. It is used for directory rename/remove events, for which
// fsnotify is not required to emit one event per child.
func (s *DB) DeletePathTree(path string) (int, error) {
	root := filepath.Clean(path)
	prefix := root + string(os.PathSeparator)

	rows, err := s.db.Query(`SELECT path FROM files`)
	if err != nil {
		return 0, fmt.Errorf("storage: list paths for tree delete: %w", err)
	}
	var paths []string
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("storage: scan path for tree delete: %w", err)
		}
		cleaned := filepath.Clean(candidate)
		if cleaned == root || strings.HasPrefix(cleaned, prefix) {
			paths = append(paths, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(paths) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("storage: begin tree delete: %w", err)
	}
	defer rollback(tx)
	for _, candidate := range paths {
		if err := deleteFileRecord(tx, candidate); err != nil {
			return 0, err
		}
	}
	if err := pruneUnreferencedChunkEmbeddings(tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("storage: commit tree delete: %w", err)
	}
	return len(paths), nil
}

func deleteFileRecord(tx *sql.Tx, path string) error {
	if _, err := tx.Exec(`DELETE FROM file_relations WHERE source_path = ? OR target_path = ?`, path, path); err != nil {
		return fmt.Errorf("storage: delete file relations: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
		return fmt.Errorf("storage: delete file record: %w", err)
	}
	return nil
}

// scanFile reads one row from a files query into a FileRecord.
func scanFile(rows *sql.Rows) (models.FileRecord, error) {
	var (
		r         models.FileRecord
		lang      string
		symsJSON  string
		impsJSON  string
		indexedAt string
	)
	if err := rows.Scan(
		&r.ID, &r.Path, &r.RelPath, &lang,
		&r.Size, &r.ModTimeUnixNano, &r.Lines, &symsJSON, &impsJSON,
		&r.Checksum, &indexedAt,
	); err != nil {
		return r, err
	}
	r.Lang = models.Lang(lang)

	// Corrupted symbols/imports JSON is a real integrity signal (bad
	// migration, manual edit) — surface it instead of silently returning
	// a FileRecord with nil slices. Callers abort on the first bad row,
	// which is what we want: a partial index is worse than a loud failure.
	if err := json.Unmarshal([]byte(symsJSON), &r.Symbols); err != nil {
		return r, fmt.Errorf("storage: decode symbols for %s: %w", r.RelPath, err)
	}
	if err := json.Unmarshal([]byte(impsJSON), &r.Imports); err != nil {
		return r, fmt.Errorf("storage: decode imports for %s: %w", r.RelPath, err)
	}
	t, err := time.Parse(time.RFC3339, indexedAt)
	if err == nil {
		r.IndexedAt = t
	}
	return r, nil
}

// ProxyLogRecord represents a persisted log of a proxy invocation.
type ProxyLogRecord struct {
	ID           int64
	Timestamp    time.Time
	Model        string
	Query        string
	TokensBefore int
	TokensAfter  int
	SavedTokens  int
	SavingsUSD   float64
}

// InsertProxyLog inserts a proxy log record into the database.
func (s *DB) InsertProxyLog(timestamp time.Time, model, query string, before, after, saved int, usd float64) error {
	_, err := s.db.Exec(`
		INSERT INTO proxy_logs (timestamp, model, query, tokens_before, tokens_after, saved_tokens, savings_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, timestamp.UTC().Format(time.RFC3339), model, query, before, after, saved, usd)
	return err
}

// GetProxyLogs retrieves the recent proxy log records (up to limit).
func (s *DB) GetProxyLogs(limit int) ([]ProxyLogRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, timestamp, model, query, tokens_before, tokens_after, saved_tokens, savings_usd
		FROM proxy_logs
		ORDER BY timestamp DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var logs []ProxyLogRecord
	for rows.Next() {
		var (
			l  ProxyLogRecord
			ts string
		)
		err := rows.Scan(&l.ID, &ts, &l.Model, &l.Query, &l.TokensBefore, &l.TokensAfter, &l.SavedTokens, &l.SavingsUSD)
		if err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err == nil {
			l.Timestamp = t
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// GetProxySummary aggregates proxy stats.
func (s *DB) GetProxySummary() (int, int, float64, error) {
	var (
		count int
		saved int
		usd   float64
	)
	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(saved_tokens), 0), COALESCE(SUM(savings_usd), 0.0)
		FROM proxy_logs
	`).Scan(&count, &saved, &usd)
	return count, saved, usd, err
}

// SaveEmbedding stores the binary embedding for a given file path.
func (s *DB) SaveEmbedding(path string, embedding []float32) error {
	return s.SaveEmbeddingWithMetadata(path, embedding, "", "", "")
}

// SaveEmbeddingWithMetadata stores a file embedding together with the source
// checksum and vector-space provenance that produced it.
func (s *DB) SaveEmbeddingWithMetadata(path string, embedding []float32, checksum, provider, model string) error {
	encoded, err := embeddings.EncodeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("storage: encode embedding: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO file_embeddings (path, embedding, checksum, provider, model)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			embedding = excluded.embedding,
			checksum = excluded.checksum,
			provider = excluded.provider,
			model = excluded.model
	`, path, encoded, checksum, provider, model)
	if err != nil {
		return fmt.Errorf("storage: save embedding: %w", err)
	}
	return nil
}

// HasFileEmbedding reports whether path has an embedding for the exact source
// checksum and vector space requested.
func (s *DB) HasFileEmbedding(path, checksum, provider, model string) (bool, error) {
	var one int
	err := s.db.QueryRow(`
		SELECT 1
		FROM file_embeddings
		WHERE path = ? AND checksum = ? AND provider = ? AND model = ?
	`, path, checksum, provider, model).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: check file embedding: %w", err)
	}
	return true, nil
}

// GetEmbedding retrieves the embedding vector for a given file path.
// Returns (nil, false, nil) if not found.
func (s *DB) GetEmbedding(path string) ([]float32, bool, error) {
	var encoded []byte
	err := s.db.QueryRow(`SELECT embedding FROM file_embeddings WHERE path = ?`, path).Scan(&encoded)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: query embedding: %w", err)
	}
	vec, err := embeddings.DecodeEmbedding(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("storage: decode embedding: %w", err)
	}
	return vec, true, nil
}

// AllEmbeddings returns a map of all file paths to their embedding vectors.
func (s *DB) AllEmbeddings() (map[string][]float32, error) {
	query := `SELECT file_embeddings.path, file_embeddings.embedding FROM file_embeddings`
	var args []any
	provider, model, configured, err := s.configuredEmbeddingProvider()
	if err != nil {
		return nil, err
	}
	if configured {
		query += `
			JOIN files ON files.path = file_embeddings.path
			WHERE file_embeddings.provider = ?
			  AND file_embeddings.model = ?
			  AND file_embeddings.checksum = files.checksum
		`
		args = append(args, provider, model)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query all embeddings: %w", err)
	}
	defer closeRows(rows)

	res := make(map[string][]float32)
	for rows.Next() {
		var (
			path    string
			encoded []byte
		)
		if err := rows.Scan(&path, &encoded); err != nil {
			return nil, fmt.Errorf("storage: scan embedding: %w", err)
		}
		vec, err := embeddings.DecodeEmbedding(encoded)
		if err != nil {
			return nil, fmt.Errorf("storage: decode embedding for %s: %w", path, err)
		}
		res[path] = vec
	}
	return res, rows.Err()
}

func (s *DB) configuredEmbeddingProvider() (provider, model string, configured bool, err error) {
	value, ok, err := s.GetMeta("embedding_provider")
	if err != nil {
		return "", "", false, fmt.Errorf("storage: get embedding provider metadata: %w", err)
	}
	if !ok {
		return "", "", false, nil
	}
	provider, model, ok = strings.Cut(value, ":")
	if !ok || provider == "" || model == "" {
		return "", "", false, fmt.Errorf("storage: invalid embedding provider metadata %q", value)
	}
	return provider, model, true, nil
}

// ClearIndex truncates all index tables in a transaction.
func (s *DB) ClearIndex() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	if _, err := tx.Exec(`DELETE FROM file_relations`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chunks`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chunk_embeddings`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM file_embeddings`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM metadata`); err != nil {
		return err
	}

	return tx.Commit()
}

// UpdateRelations replaces all records in the file_relations table with the new set.
func (s *DB) UpdateRelations(relations []models.FileRelation) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin update relations: %w", err)
	}
	defer rollback(tx)

	if _, err := tx.Exec(`DELETE FROM file_relations`); err != nil {
		return fmt.Errorf("storage: clear relations: %w", err)
	}

	stmt, err := tx.Prepare(`
		INSERT INTO file_relations (source_path, target_path, rel_type)
		VALUES (?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("storage: prepare insert relation: %w", err)
	}
	defer closeStmt(stmt)

	for _, r := range relations {
		if _, err := stmt.Exec(r.SourcePath, r.TargetPath, r.RelType); err != nil {
			return fmt.Errorf("storage: insert relation (%s -> %s): %w", r.SourcePath, r.TargetPath, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit relations: %w", err)
	}
	return nil
}

// GetRelationsForSource returns all relations originating from sourcePath.
func (s *DB) GetRelationsForSource(sourcePath string) ([]models.FileRelation, error) {
	rows, err := s.db.Query(`
		SELECT source_path, target_path, rel_type
		FROM file_relations
		WHERE source_path = ?
	`, sourcePath)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var rels []models.FileRelation
	for rows.Next() {
		var r models.FileRelation
		if err := rows.Scan(&r.SourcePath, &r.TargetPath, &r.RelType); err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	return rels, rows.Err()
}

// GetRelationsForTarget returns all relations targeting targetPath.
func (s *DB) GetRelationsForTarget(targetPath string) ([]models.FileRelation, error) {
	rows, err := s.db.Query(`
		SELECT source_path, target_path, rel_type
		FROM file_relations
		WHERE target_path = ?
	`, targetPath)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var rels []models.FileRelation
	for rows.Next() {
		var r models.FileRelation
		if err := rows.Scan(&r.SourcePath, &r.TargetPath, &r.RelType); err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	return rels, rows.Err()
}

// AllRelations returns every FileRelation in the database.
func (s *DB) AllRelations() ([]models.FileRelation, error) {
	rows, err := s.db.Query(`
		SELECT source_path, target_path, rel_type
		FROM file_relations
	`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var rels []models.FileRelation
	for rows.Next() {
		var r models.FileRelation
		if err := rows.Scan(&r.SourcePath, &r.TargetPath, &r.RelType); err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	return rels, rows.Err()
}

// UpdateChunks updates the chunks associated with a file path inside a transaction.
func (s *DB) UpdateChunks(filePath string, chunks []models.Chunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin tx for UpdateChunks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := replaceChunksInTx(tx, filePath, chunks); err != nil {
		return err
	}
	if err := pruneUnreferencedChunkEmbeddings(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit chunks: %w", err)
	}
	return nil
}

func replaceChunksInTx(tx *sql.Tx, filePath string, chunks []models.Chunk) error {
	if _, err := tx.Exec(`DELETE FROM chunks WHERE file_path = ?`, filePath); err != nil {
		return fmt.Errorf("storage: delete old chunks: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
		INSERT INTO chunks (
			file_path, chunk_id, parent_id, kind, symbol,
			start_line, end_line, content_hash, ast_hash,
			heading_path, calls, token_estimate, indexed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("storage: prepare chunk insert: %w", err)
	}
	defer closeStmt(stmt)

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for _, c := range chunks {
		calls, err := json.Marshal(c.Calls)
		if err != nil {
			return fmt.Errorf("storage: marshal calls for chunk %s: %w", c.ChunkID, err)
		}
		_, err = stmt.Exec(
			filePath, c.ChunkID, c.ParentID, c.Kind, c.Symbol,
			c.StartLine, c.EndLine, c.ContentHash, c.ASTHash,
			c.HeadingPath, string(calls), c.TokenEstimate, nowStr,
		)
		if err != nil {
			return fmt.Errorf("storage: insert chunk %s: %w", c.ChunkID, err)
		}
	}
	return nil
}

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func pruneUnreferencedChunkEmbeddings(exec sqlExecer) error {
	_, err := exec.Exec(`
		DELETE FROM chunk_embeddings
		WHERE NOT EXISTS (
			SELECT 1
			FROM chunks
			WHERE chunks.content_hash = chunk_embeddings.content_hash
		)
	`)
	if err != nil {
		return fmt.Errorf("storage: prune unreferenced chunk embeddings: %w", err)
	}
	return nil
}

// PruneUnreferencedChunkEmbeddings removes cached vectors no longer referenced
// by any current chunk. Full scans call this once after publishing every file
// generation rather than rescanning the global cache once per file.
func (s *DB) PruneUnreferencedChunkEmbeddings() error {
	return pruneUnreferencedChunkEmbeddings(s.db)
}

// SaveChunkEmbedding stores the binary embedding for a given content hash.
func (s *DB) SaveChunkEmbedding(contentHash string, embedding []float32, provider, model string) error {
	encoded, err := embeddings.EncodeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("storage: encode chunk embedding: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO chunk_embeddings (content_hash, embedding, provider, model, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO UPDATE SET embedding = excluded.embedding, provider = excluded.provider, model = excluded.model, created_at = excluded.created_at
	`, contentHash, encoded, provider, model, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("storage: save chunk embedding: %w", err)
	}
	return nil
}

// GetChunkEmbedding retrieves the embedding vector for a given content hash.
// Returns (nil, false, nil) if not found.
func (s *DB) GetChunkEmbedding(contentHash string) ([]float32, bool, error) {
	return s.getChunkEmbedding(contentHash, "", "", false)
}

// GetChunkEmbeddingForProvider retrieves a cached chunk vector only when its
// provider and model match the requested vector space.
func (s *DB) GetChunkEmbeddingForProvider(contentHash, provider, model string) ([]float32, bool, error) {
	return s.getChunkEmbedding(contentHash, provider, model, true)
}

func (s *DB) getChunkEmbedding(contentHash, provider, model string, filterProvider bool) ([]float32, bool, error) {
	var encoded []byte
	query := `SELECT embedding FROM chunk_embeddings WHERE content_hash = ?`
	args := []any{contentHash}
	if filterProvider {
		query += ` AND provider = ? AND model = ?`
		args = append(args, provider, model)
	}
	err := s.db.QueryRow(query, args...).Scan(&encoded)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: get chunk embedding: %w", err)
	}
	decoded, err := embeddings.DecodeEmbedding(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("storage: decode chunk embedding: %w", err)
	}
	return decoded, true, nil
}

// AllChunkEmbeddings returns all cached chunk embeddings keyed by content hash.
func (s *DB) AllChunkEmbeddings() (map[string][]float32, error) {
	query := `SELECT content_hash, embedding FROM chunk_embeddings`
	var args []any
	provider, model, configured, err := s.configuredEmbeddingProvider()
	if err != nil {
		return nil, err
	}
	if configured {
		query += ` WHERE provider = ? AND model = ?`
		args = append(args, provider, model)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query chunk embeddings: %w", err)
	}
	defer closeRows(rows)

	res := make(map[string][]float32)
	for rows.Next() {
		var (
			hash    string
			encoded []byte
		)
		if err := rows.Scan(&hash, &encoded); err != nil {
			return nil, fmt.Errorf("storage: scan chunk embedding: %w", err)
		}
		vec, err := embeddings.DecodeEmbedding(encoded)
		if err != nil {
			return nil, fmt.Errorf("storage: decode chunk embedding for %s: %w", hash, err)
		}
		res[hash] = vec
	}
	return res, rows.Err()
}

// GetChunksForFile retrieves all chunks for a given file path.
func (s *DB) GetChunksForFile(filePath string) ([]models.Chunk, error) {
	return s.SearchChunks(ChunkSearchOptions{FilePath: filePath})
}

// AllChunks retrieves every chunk in deterministic file/line order.
func (s *DB) AllChunks() ([]models.Chunk, error) {
	return s.SearchChunks(ChunkSearchOptions{})
}

// SearchChunks retrieves chunks by file path, symbol substring, or content hash.
func (s *DB) SearchChunks(opts ChunkSearchOptions) ([]models.Chunk, error) {
	headingPath := "heading_path"
	if !s.hasHeadingPath {
		// Pre-migration index opened read-only: no such column to select.
		headingPath = "'' AS heading_path"
	}
	query := `
		SELECT id, file_path, chunk_id, parent_id, kind, symbol, start_line, end_line, content_hash, ast_hash, ` + headingPath + `, calls, token_estimate, indexed_at
		FROM chunks
	`
	var where []string
	var args []any
	if opts.FilePath != "" {
		where = append(where, "file_path = ?")
		args = append(args, opts.FilePath)
	}
	if opts.Symbol != "" {
		where = append(where, "LOWER(symbol) LIKE LOWER(?)")
		args = append(args, "%"+opts.Symbol+"%")
	}
	if opts.ContentHash != "" {
		where = append(where, "content_hash = ?")
		args = append(args, opts.ContentHash)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY file_path ASC, start_line ASC, chunk_id ASC"
	if opts.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, opts.Limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: get chunks: %w", err)
	}
	defer closeRows(rows)

	return scanChunks(rows)
}

func scanChunks(rows *sql.Rows) ([]models.Chunk, error) {
	var chunks []models.Chunk
	for rows.Next() {
		var c models.Chunk
		var indexedAtStr string
		var callsJSON string
		err := rows.Scan(
			&c.ID, &c.FilePath, &c.ChunkID, &c.ParentID, &c.Kind, &c.Symbol,
			&c.StartLine, &c.EndLine, &c.ContentHash, &c.ASTHash, &c.HeadingPath,
			&callsJSON, &c.TokenEstimate, &indexedAtStr,
		)
		if err != nil {
			return nil, fmt.Errorf("storage: scan chunk: %w", err)
		}
		if err := json.Unmarshal([]byte(callsJSON), &c.Calls); err != nil {
			return nil, fmt.Errorf("storage: decode calls for chunk %s: %w", c.ChunkID, err)
		}
		c.IndexedAt, _ = time.Parse(time.RFC3339, indexedAtStr)
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}
