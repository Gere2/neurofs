package indexer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func TestWatcherIncrementalIndexing(t *testing.T) {
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
	defer db.Close()

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
	defer w.Close()

	// Wait for watcher to register existing directories
	time.Sleep(100 * time.Millisecond)

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

	// Sleep to let fsnotify fire and watcher debounce (200ms) process it
	time.Sleep(400 * time.Millisecond)

	files, err := db.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}

	found := false
	for _, f := range files {
		if f.RelPath == "helper.go" {
			found = true
			if f.Size != int64(len(content1)) {
				t.Errorf("expected size %d, got %d", len(content1), f.Size)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected helper.go to be incrementally indexed")
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

	time.Sleep(400 * time.Millisecond)

	files, err = db.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}

	found = false
	for _, f := range files {
		if f.RelPath == "helper.go" {
			found = true
			if f.Size != int64(len(content2)) {
				t.Errorf("expected updated size %d, got %d", len(content2), f.Size)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected helper.go to still be indexed after modification")
	}

	// Step 3: Delete the file
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("Remove file: %v", err)
	}

	time.Sleep(400 * time.Millisecond)

	files, err = db.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}

	for _, f := range files {
		if f.RelPath == "helper.go" {
			t.Errorf("expected helper.go to be deleted from index")
		}
	}
}
