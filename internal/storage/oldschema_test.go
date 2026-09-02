package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// oldChunksSchema is the chunks table exactly as binaries before the
// heading_path migration wrote it.
const oldChunksSchema = `
CREATE TABLE files (
    path      TEXT PRIMARY KEY,
    rel_path  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE chunks (
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
    calls          TEXT    NOT NULL DEFAULT '[]',
    token_estimate INTEGER NOT NULL DEFAULT 0,
    indexed_at     TEXT    NOT NULL
);
INSERT INTO files (path, rel_path) VALUES ('/repo/auth.go', 'auth.go');
INSERT INTO chunks (file_path, chunk_id, parent_id, kind, symbol, start_line, end_line, content_hash, ast_hash, calls, token_estimate, indexed_at)
VALUES ('/repo/auth.go', 'func:login', '', 'func', 'Login', 1, 10, 'abc', 'def', '[]', 42, '2026-01-01T00:00:00Z');
`

func writeOldSchemaIndex(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(oldChunksSchema); err != nil {
		_ = raw.Close()
		t.Fatalf("apply old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath
}

// TestReadOnlyOpenToleratesPreHeadingPathIndex pins the contract that broke
// when heading_path was added: OpenReadOnly deliberately skips migrations, so
// gate, cross-shape verification and every --disable-index-refresh
// measurement can be pointed at an index an older binary wrote. Selecting the
// new column unconditionally made all of them fail with "no such column:
// heading_path" instead of measuring.
func TestReadOnlyOpenToleratesPreHeadingPathIndex(t *testing.T) {
	dbPath := writeOldSchemaIndex(t)

	db, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = db.Close() }()

	if db.hasHeadingPath {
		t.Fatal("an index without the column must not be treated as having it")
	}
	chunks, err := db.AllChunks()
	if err != nil {
		t.Fatalf("AllChunks against a pre-migration index: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Symbol != "Login" {
		t.Errorf("symbol = %q, want Login", chunks[0].Symbol)
	}
	if chunks[0].HeadingPath != "" {
		t.Errorf("heading path must degrade to empty, got %q", chunks[0].HeadingPath)
	}
}

// TestReadWriteOpenMigratesPreHeadingPathIndex is the other half: a normal
// open adds the column back, so the next scan can populate it. It starts from
// the real current schema and drops the column, rather than from a
// hand-written stub, so the migration is exercised against a genuine index.
func TestReadWriteOpenMigratesPreHeadingPathIndex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !db.hasHeadingPath {
		t.Fatal("a fresh index must have the column")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE chunks DROP COLUMN heading_path`); err != nil {
		_ = raw.Close()
		t.Fatalf("drop column to simulate the older schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	regressed, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	if regressed.hasHeadingPath {
		t.Error("the dropped column must be detected as absent")
	}
	if _, err := regressed.AllChunks(); err != nil {
		t.Errorf("read-only chunk read must still work: %v", err)
	}
	if err := regressed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	migrated, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen read-write: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	if !migrated.hasHeadingPath {
		t.Fatal("a read-write open must migrate the column back in")
	}
	if _, err := migrated.AllChunks(); err != nil {
		t.Errorf("AllChunks after migration: %v", err)
	}
}
