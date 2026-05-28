// Package storage manages NeuroFS's SQLite index.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/models"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    path       TEXT    NOT NULL UNIQUE,
    rel_path   TEXT    NOT NULL,
    lang       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
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

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("storage: open sqlite: %w", err)
	}

	// busy_timeout must be set before journal_mode so the WAL switch
	// itself waits when another process is mid-switch. WAL lets readers
	// proceed during writes; synchronous=NORMAL is the documented safe
	// pair for WAL. Without these, two concurrent `neurofs scan`
	// invocations collide instantly with SQLITE_BUSY.
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("storage: %s: %w", p, err)
		}
	}

	db.SetMaxOpenConns(1) // SQLite is single-writer

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("storage: apply schema: %w", err)
	}

	return &DB{db: db, path: dbPath}, nil
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
		INSERT INTO files (path, rel_path, lang, size, lines, symbols, imports, checksum, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			rel_path   = excluded.rel_path,
			lang       = excluded.lang,
			size       = excluded.size,
			lines      = excluded.lines,
			symbols    = excluded.symbols,
			imports    = excluded.imports,
			checksum   = excluded.checksum,
			indexed_at = excluded.indexed_at
	`,
		f.Path, f.RelPath, string(f.Lang),
		f.Size, f.Lines,
		string(syms), string(imps),
		f.Checksum, f.IndexedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("storage: upsert %s: %w", f.RelPath, err)
	}
	return nil
}

// AllFiles returns every FileRecord in the index.
func (s *DB) AllFiles() ([]models.FileRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, path, rel_path, lang, size, lines, symbols, imports, checksum, indexed_at
		FROM files
		ORDER BY rel_path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
		SELECT id, path, rel_path, lang, size, lines, symbols, imports, checksum, indexed_at
		FROM files
		WHERE rel_path = ?
	`, relPath)
	if err != nil {
		return models.FileRecord{}, err
	}
	defer rows.Close()

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
	defer rows.Close()

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
	defer rows.Close()

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
	rows.Close()

	if len(toDelete) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("storage: begin tx: %w", err)
	}
	stmt, err := tx.Prepare(`DELETE FROM files WHERE path = ?`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("storage: prepare delete: %w", err)
	}
	defer stmt.Close()
	for _, p := range toDelete {
		if _, err := stmt.Exec(p); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("storage: delete %s: %w", p, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("storage: commit delete: %w", err)
	}
	return len(toDelete), nil
}

// DeleteFile deletes a single file record by path.
func (s *DB) DeleteFile(path string) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE path = ?`, path)
	return err
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
		&r.Size, &r.Lines, &symsJSON, &impsJSON,
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
	defer rows.Close()

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
	encoded, err := embeddings.EncodeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("storage: encode embedding: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO file_embeddings (path, embedding)
		VALUES (?, ?)
		ON CONFLICT(path) DO UPDATE SET embedding = excluded.embedding
	`, path, encoded)
	if err != nil {
		return fmt.Errorf("storage: save embedding: %w", err)
	}
	return nil
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
	rows, err := s.db.Query(`SELECT path, embedding FROM file_embeddings`)
	if err != nil {
		return nil, fmt.Errorf("storage: query all embeddings: %w", err)
	}
	defer rows.Close()

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

// ClearIndex truncates all index tables in a transaction.
func (s *DB) ClearIndex() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
	defer tx.Rollback()

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
	defer stmt.Close()

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
	defer rows.Close()

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
	defer rows.Close()

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
	defer rows.Close()

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

	_, err = tx.Exec(`DELETE FROM chunks WHERE file_path = ?`, filePath)
	if err != nil {
		return fmt.Errorf("storage: delete old chunks: %w", err)
	}

	if len(chunks) == 0 {
		return tx.Commit()
	}

	stmt, err := tx.Prepare(`
		INSERT INTO chunks (
			file_path, chunk_id, parent_id, kind, symbol,
			start_line, end_line, content_hash, ast_hash,
			token_estimate, indexed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("storage: prepare chunk insert: %w", err)
	}
	defer stmt.Close()

	nowStr := time.Now().UTC().Format(time.RFC3339)
	for _, c := range chunks {
		_, err = stmt.Exec(
			filePath, c.ChunkID, c.ParentID, c.Kind, c.Symbol,
			c.StartLine, c.EndLine, c.ContentHash, c.ASTHash,
			c.TokenEstimate, nowStr,
		)
		if err != nil {
			return fmt.Errorf("storage: insert chunk %s: %w", c.ChunkID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit chunks: %w", err)
	}
	return nil
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
	`, contentHash, encoded, provider, model, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("storage: save chunk embedding: %w", err)
	}
	return nil
}

// GetChunkEmbedding retrieves the embedding vector for a given content hash.
// Returns (nil, false, nil) if not found.
func (s *DB) GetChunkEmbedding(contentHash string) ([]float32, bool, error) {
	var encoded []byte
	err := s.db.QueryRow(`SELECT embedding FROM chunk_embeddings WHERE content_hash = ?`, contentHash).Scan(&encoded)
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
	rows, err := s.db.Query(`SELECT content_hash, embedding FROM chunk_embeddings`)
	if err != nil {
		return nil, fmt.Errorf("storage: query chunk embeddings: %w", err)
	}
	defer rows.Close()

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
	query := `
		SELECT id, file_path, chunk_id, parent_id, kind, symbol, start_line, end_line, content_hash, ast_hash, token_estimate, indexed_at
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
	defer rows.Close()

	return scanChunks(rows)
}

func scanChunks(rows *sql.Rows) ([]models.Chunk, error) {
	var chunks []models.Chunk
	for rows.Next() {
		var c models.Chunk
		var indexedAtStr string
		err := rows.Scan(
			&c.ID, &c.FilePath, &c.ChunkID, &c.ParentID, &c.Kind, &c.Symbol,
			&c.StartLine, &c.EndLine, &c.ContentHash, &c.ASTHash, &c.TokenEstimate,
			&indexedAtStr,
		)
		if err != nil {
			return nil, fmt.Errorf("storage: scan chunk: %w", err)
		}
		c.IndexedAt, _ = time.Parse(time.RFC3339, indexedAtStr)
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}
