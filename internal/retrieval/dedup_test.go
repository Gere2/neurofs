package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

func TestDedupeSameContentKeepsHighestScoringPath(t *testing.T) {
	hits := []Hit{
		{Path: "output/pdf/build_poster.py", Symbol: "Poster", Score: 42, ContentHash: "aaa"},
		{Path: "src/main.py", Symbol: "Runner", Score: 30, ContentHash: "bbb"},
		{Path: "tmp/pdfs/build_poster.py", Symbol: "Poster", Score: 21, ContentHash: "aaa"},
		{Path: "vendor/copy/build_poster.py", Symbol: "Poster", Score: 9, ContentHash: "aaa"},
	}

	got := dedupeSameContent(hits)

	if len(got) != 2 {
		t.Fatalf("expected 2 hits after content dedupe, got %d: %+v", len(got), got)
	}
	if got[0].Path != "output/pdf/build_poster.py" {
		t.Errorf("expected the highest-scoring copy to survive, got %q", got[0].Path)
	}
	if got[0].Score != 42 {
		t.Errorf("kept hit must retain its own score, got %v", got[0].Score)
	}
	wantAlsoAt := []string{"tmp/pdfs/build_poster.py", "vendor/copy/build_poster.py"}
	if !reflect.DeepEqual(got[0].AlsoAt, wantAlsoAt) {
		t.Errorf("also_at = %v, want %v", got[0].AlsoAt, wantAlsoAt)
	}
	if got[1].Path != "src/main.py" {
		t.Errorf("distinct content must survive, got %q", got[1].Path)
	}
	if len(got[1].AlsoAt) != 0 {
		t.Errorf("unique content must not carry also_at, got %v", got[1].AlsoAt)
	}
}

func TestDedupeSameContentIgnoresEmptyHashAndSamePath(t *testing.T) {
	hits := []Hit{
		{Path: "a.go", Score: 10},
		{Path: "b.go", Score: 9},
		{Path: "c.go", Symbol: "Dup", Score: 8, ContentHash: "h"},
		{Path: "c.go", Symbol: "Dup2", Score: 7, ContentHash: "h"},
	}

	got := dedupeSameContent(hits)

	if len(got) != 3 {
		t.Fatalf("expected 3 hits, got %d: %+v", len(got), got)
	}
	for _, h := range got[:2] {
		if len(h.AlsoAt) != 0 {
			t.Errorf("hash-less hits must never be folded: %+v", h)
		}
	}
	if len(got[2].AlsoAt) != 0 {
		t.Errorf("a duplicate from the same path adds no new location, got %v", got[2].AlsoAt)
	}
}

// TestDedupeSameContentEndToEnd indexes the real duplicate shape observed on
// raiz-app — one generated file mirrored at two paths — and asserts the
// bundle-facing search serves it once.
func TestDedupeSameContentEndToEnd(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	write := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(repo, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
	}
	poster := "package poster\n\nfunc BuildPosterDocument(title string) string {\n\treturn title\n}\n"
	write("output/build_poster.go", poster)
	write("tmp/build_poster.go", poster)

	cfg, err := config.New(repo)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatalf("indexer.Run: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}

	resp, err := Search(context.Background(), Options{
		Query: "BuildPosterDocument",
		Repo:  repo,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	seen := make(map[string]int)
	for _, hit := range resp.Results {
		seen[hit.ContentHash]++
	}
	for hash, count := range seen {
		if hash != "" && count > 1 {
			t.Fatalf("content hash %s served %d times: %+v", hash, count, resp.Results)
		}
	}
	var folded bool
	for _, hit := range resp.Results {
		if len(hit.AlsoAt) > 0 {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("expected the duplicate path to be recorded in also_at: %+v", resp.Results)
	}
}
