// Package storage manages NeuroFS's SQLite index.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
`

// DB wraps a SQLite connection and provides typed read/write operations.
type DB struct {
	db   *sql.DB
	path string
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
	return err
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

// DeleteRemovedFiles deletes records whose paths are no longer present on disk.
// It returns the number of records deleted.
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

	for _, p := range toDelete {
		if _, err := s.db.Exec(`DELETE FROM files WHERE path = ?`, p); err != nil {
			return 0, err
		}
	}
	return len(toDelete), nil
}

// scanFile reads one row from a files query into a FileRecord.
func scanFile(rows *sql.Rows) (models.FileRecord, error) {
	var (
		r        models.FileRecord
		lang     string
		symsJSON string
		impsJSON string
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

	if err := json.Unmarshal([]byte(symsJSON), &r.Symbols); err != nil {
		r.Symbols = nil
	}
	if err := json.Unmarshal([]byte(impsJSON), &r.Imports); err != nil {
		r.Imports = nil
	}
	t, err := time.Parse(time.RFC3339, indexedAt)
	if err == nil {
		r.IndexedAt = t
	}
	return r, nil
}
