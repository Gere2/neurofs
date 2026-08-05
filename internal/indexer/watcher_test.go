package indexer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/storage"
)

func waitForIndexedPath(t *testing.T, db *storage.DB, relPath string, wantPresent bool) *models.FileRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, err := db.AllFiles()
		if err != nil {
			t.Fatalf("AllFiles: %v", err)
		}
		for i := range files {
			if files[i].RelPath == relPath {
				if wantPresent {
					return &files[i]
				}
				break
			}
		}
		if !wantPresent {
			found := false
			for i := range files {
				if files[i].RelPath == relPath {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if wantPresent {
		t.Fatalf("timed out waiting for %s to be indexed", relPath)
	}
	t.Fatalf("timed out waiting for %s to be removed", relPath)
	return nil
}

func TestWatcherIncrementalIndexing(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "index.db")

	cfg := &config.Config{
		RepoRoot: tmpDir,
		DBPath:   dbPath,
		Budget:   8000,
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	// Initial scan to make sure DB schema is setup
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatalf("indexer run: %v", err)
	}

	var logMu sync.Mutex
	logs := make([]string, 0)
	logf := func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	w, err := indexer.NewWatcher(cfg, db, logf)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("watcher start: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("close watcher: %v", err)
		}
	})

	// Step 1: Create a new supported file
	filePath := filepath.Join(tmpDir, "helper.go")
	content1 := `package main
import "fmt"
func Help() {
	fmt.Println("helping")
}`
	if err := os.WriteFile(filePath, []byte(content1), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	record := waitForIndexedPath(t, db, "helper.go", true)
	if record.Size != int64(len(content1)) {
		t.Errorf("expected size %d, got %d", len(content1), record.Size)
	}

	// Step 2: Modify the file
	content2 := `package main
import "fmt"
func Help() {
	fmt.Println("helping more")
}`
	if err := os.WriteFile(filePath, []byte(content2), 0o644); err != nil {
		t.Fatalf("WriteFile modify: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		record = waitForIndexedPath(t, db, "helper.go", true)
		if record.Size == int64(len(content2)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected updated size %d, got %d", len(content2), record.Size)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Step 3: Delete the file
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("Remove file: %v", err)
	}

	waitForIndexedPath(t, db, "helper.go", false)
}

func TestWatcherIndexesMovedDirectoryTreeAndRemovesDescendants(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repoRoot := t.TempDir()
	dbPath := filepath.Join(repoRoot, ".neurofs", "index.db")
	cfg := &config.Config{RepoRoot: repoRoot, DBPath: dbPath, Budget: 8000}

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatalf("indexer run: %v", err)
	}

	w, err := indexer.NewWatcher(cfg, db, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("watcher start: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("close watcher: %v", err)
		}
	})

	stagingRoot := t.TempDir()
	stagedTree := filepath.Join(stagingRoot, "incoming")
	stagedFile := filepath.Join(stagedTree, "pkg", "nested.go")
	if err := os.MkdirAll(filepath.Dir(stagedFile), 0o755); err != nil {
		t.Fatalf("mkdir staged tree: %v", err)
	}
	const initial = "package pkg\n\nfunc BeforeMove() {}\n"
	if err := os.WriteFile(stagedFile, []byte(initial), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}

	movedTree := filepath.Join(repoRoot, "incoming")
	if err := os.Rename(stagedTree, movedTree); err != nil {
		t.Fatalf("move tree into repo: %v", err)
	}
	relPath := filepath.Join("incoming", "pkg", "nested.go")
	record := waitForIndexedPath(t, db, relPath, true)
	if record.Size != int64(len(initial)) {
		t.Fatalf("moved file size = %d, want %d", record.Size, len(initial))
	}

	// Descendant directories must also be watched after the move.
	const changed = "package pkg\n\nfunc AfterMoveAndEdit() {}\n"
	movedFile := filepath.Join(movedTree, "pkg", "nested.go")
	if err := os.WriteFile(movedFile, []byte(changed), 0o644); err != nil {
		t.Fatalf("modify moved file: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		record = waitForIndexedPath(t, db, relPath, true)
		if record.Size == int64(len(changed)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("moved descendant was not updated: size = %d", record.Size)
		}
		time.Sleep(25 * time.Millisecond)
	}

	if err := os.RemoveAll(movedTree); err != nil {
		t.Fatalf("remove moved tree: %v", err)
	}
	waitForIndexedPath(t, db, relPath, false)
}
