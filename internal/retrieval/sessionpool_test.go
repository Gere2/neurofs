package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func newPoolTestRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "parser.go"),
		[]byte("package main\n\nfunc ParseFunction(input string) string {\n\treturn input\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSessionPoolReusesUntilIndexChanges(t *testing.T) {
	repo := newPoolTestRepo(t)
	ctx := context.Background()

	first, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	second, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	if first != second {
		t.Fatal("unchanged index must reuse the pooled session")
	}

	// Simulate a rescan: bump the index database's mtime.
	dbPath := filepath.Join(repo, config.DirName, config.DBName)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dbPath, future, future); err != nil {
		t.Fatal(err)
	}

	third, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("third session: %v", err)
	}
	if third == first {
		t.Fatal("index mtime change must rebuild the pooled session")
	}
}

func TestSearchSharedEndToEnd(t *testing.T) {
	repo := newPoolTestRepo(t)
	resp, err := SearchShared(context.Background(), Options{Query: "ParseFunction", Repo: repo, Limit: 3})
	if err != nil {
		t.Fatalf("search shared: %v", err)
	}
	if len(resp.Results) == 0 || resp.Results[0].Symbol != "ParseFunction" {
		t.Fatalf("results = %+v, want ParseFunction on top", resp.Results)
	}
}
