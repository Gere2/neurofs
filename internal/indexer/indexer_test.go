package indexer

import (
	"os"
	"path/filepath"
	"strings"
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

func TestScanPersistsStableGoChunks(t *testing.T) {
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

	filePath := filepath.Join(tempDir, "service.go")
	initial := `package service

type Options struct {
	Enabled bool
}

func Alpha() string {
	return "one"
}

func Beta() string {
	return "two"
}
`
	if err := os.WriteFile(filePath, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	stats, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if stats.Chunks < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", stats.Chunks)
	}

	before := chunkHashesBySymbol(t, db, filePath)
	for _, symbol := range []string{"Options", "Alpha", "Beta"} {
		if before[symbol] == "" {
			t.Fatalf("missing chunk for %s in %#v", symbol, before)
		}
	}

	updated := strings.Replace(initial, `return "one"`, `return "ONE"`, 1)
	if err := os.WriteFile(filePath, []byte(updated), 0o644); err != nil {
		t.Fatalf("write updated file: %v", err)
	}
	future := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(filePath, future, future); err != nil {
		t.Fatalf("set updated mtime: %v", err)
	}

	if _, err := Run(cfg, db, Options{}); err != nil {
		t.Fatalf("rescan failed: %v", err)
	}

	after := chunkHashesBySymbol(t, db, filePath)
	if before["Alpha"] == after["Alpha"] {
		t.Fatalf("expected Alpha chunk hash to change")
	}
	if before["Beta"] != after["Beta"] {
		t.Fatalf("expected Beta chunk hash to stay stable: before=%s after=%s", before["Beta"], after["Beta"])
	}
	if before["Options"] != after["Options"] {
		t.Fatalf("expected Options chunk hash to stay stable: before=%s after=%s", before["Options"], after["Options"])
	}
}

func chunkHashesBySymbol(t *testing.T, db *storage.DB, filePath string) map[string]string {
	t.Helper()
	chunks, err := db.GetChunksForFile(filePath)
	if err != nil {
		t.Fatalf("get chunks: %v", err)
	}
	hashes := make(map[string]string, len(chunks))
	for _, c := range chunks {
		hashes[c.Symbol] = c.ContentHash
		if c.ChunkID == "" {
			t.Fatalf("chunk for %s has empty chunk id", c.Symbol)
		}
		if c.StartLine < 1 || c.EndLine < c.StartLine {
			t.Fatalf("invalid line range for %s: %d-%d", c.Symbol, c.StartLine, c.EndLine)
		}
	}
	return hashes
}

func TestProviderChangeInvalidatesIndex(t *testing.T) {
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

	// Create test file
	file1 := filepath.Join(tempDir, "file1.go")
	if err := os.WriteFile(file1, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}

	// 1. Initial run (default provider/model = "mock:mock-lcg")
	os.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	stats1, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("first scan failed: %v", err)
	}
	if stats1.Indexed != 1 {
		t.Errorf("expected 1 file indexed, got %d", stats1.Indexed)
	}

	// Verify embedding provider is stored in metadata
	providerVal, ok, err := db.GetMeta("embedding_provider")
	if err != nil || !ok || providerVal != "mock:mock-lcg" {
		t.Errorf("expected stored provider mock:mock-lcg, got %q (ok=%v, err=%v)", providerVal, ok, err)
	}

	// 2. Run again with the same provider -> should be cached
	stats2, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("second scan failed: %v", err)
	}
	if stats2.Cached != 1 {
		t.Errorf("expected 1 file cached, got %d", stats2.Cached)
	}

	// 3. Run again after changing provider env to "openai" (even though API key is mock, we just want to test provider string change detection)
	os.Setenv("NEUROFS_EMBEDDING_PROVIDER", "openai")
	os.Setenv("OPENAI_API_KEY", "sk-dummy-key")
	defer func() {
		os.Unsetenv("NEUROFS_EMBEDDING_PROVIDER")
		os.Unsetenv("OPENAI_API_KEY")
	}()

	stats3, err := Run(cfg, db, Options{})
	if err != nil {
		t.Fatalf("third scan failed: %v", err)
	}

	// Since provider changed, index should have been cleared, leading to full re-indexing of file1 (not cached).
	if stats3.Indexed != 1 {
		t.Errorf("expected 1 file indexed due to provider change invalidation, got %d", stats3.Indexed)
	}
	if stats3.Cached != 0 {
		t.Errorf("expected 0 files cached, got %d", stats3.Cached)
	}

	// Verify new provider is stored in metadata
	newProviderVal, ok, err := db.GetMeta("embedding_provider")
	if err != nil || !ok || newProviderVal != "openai:text-embedding-3-small" {
		t.Errorf("expected stored provider openai:text-embedding-3-small, got %q", newProviderVal)
	}
}
