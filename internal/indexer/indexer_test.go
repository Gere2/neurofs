package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func TestIncrementalIndexing(t *testing.T) {
	tempDir := t.TempDir()

	cfg, err := config.New(tempDir)
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Create test files
	file1 := filepath.Join(tempDir, "file1.go")
	if err := os.WriteFile(file1, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}

	file2 := filepath.Join(tempDir, "file2.go")
	if err := os.WriteFile(file2, []byte("package main\n\nfunc hello() {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}

	// Set modification times in the past to ensure they are less than indexing time
	now := time.Now().UTC()
	pastTime := now.Add(-10 * time.Second)
	if err := os.Chtimes(file1, pastTime, pastTime); err != nil {
		t.Fatalf("failed to set mtime for file1: %v", err)
	}
	if err := os.Chtimes(file2, pastTime, pastTime); err != nil {
		t.Fatalf("failed to set mtime for file2: %v", err)
	}

	// 1. First run: should index both files
	stats1, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("first scan failed: %v", err)
	}

	if stats1.Indexed != 2 {
		t.Errorf("expected 2 files indexed, got %d", stats1.Indexed)
	}
	if stats1.Cached != 0 {
		t.Errorf("expected 0 files cached on first run, got %d", stats1.Cached)
	}

	// 2. Second run: nothing changed, should skip both files
	stats2, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("second scan failed: %v", err)
	}

	if stats2.Indexed != 0 {
		t.Errorf("expected 0 files indexed on second run, got %d", stats2.Indexed)
	}
	if stats2.Cached != 2 {
		t.Errorf("expected 2 files cached on second run, got %d", stats2.Cached)
	}

	// 3. Third run: modify file1 (change size and/or content) and set mtime to the future
	if err := os.WriteFile(file1, []byte("package main\n\nfunc main() { println(\"hello\") }\n"), 0o644); err != nil {
		t.Fatalf("failed to modify file1: %v", err)
	}
	futureTime := now.Add(10 * time.Second)
	if err := os.Chtimes(file1, futureTime, futureTime); err != nil {
		t.Fatalf("failed to set future mtime for file1: %v", err)
	}

	stats3, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("third scan failed: %v", err)
	}

	if stats3.Indexed != 1 {
		t.Errorf("expected 1 file indexed on third run, got %d", stats3.Indexed)
	}
	if stats3.Cached != 1 {
		t.Errorf("expected 1 file cached on third run, got %d", stats3.Cached)
	}

	// 4. Fourth run: delete file2, should remove it from the DB
	if err := os.Remove(file2); err != nil {
		t.Fatalf("failed to delete file2: %v", err)
	}
	// Reset file1's modification time to the past so it is <= run 3's IndexedAt
	pastTimeForFile1 := now.Add(-1 * time.Second)
	if err := os.Chtimes(file1, pastTimeForFile1, pastTimeForFile1); err != nil {
		t.Fatalf("failed to reset mtime for file1: %v", err)
	}

	stats4, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("fourth scan failed: %v", err)
	}

	if stats4.Removed != 1 {
		t.Errorf("expected 1 file removed, got %d", stats4.Removed)
	}
	if stats4.Cached != 1 {
		t.Errorf("expected 1 file cached on fourth run, got %d", stats4.Cached)
	}

	// Check that file2 is no longer in database
	files, err := db.AllFiles()
	if err != nil {
		t.Fatalf("failed to query all files: %v", err)
	}

	for _, f := range files {
		if f.Path == file2 {
			t.Errorf("file2 was not removed from the database")
		}
	}
}
