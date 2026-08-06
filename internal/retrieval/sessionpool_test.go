package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
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
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close index: %v", err)
		}
	}()
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

func TestSessionPoolRebuildsOnWALWrite(t *testing.T) {
	repo := newPoolTestRepo(t)
	ctx := context.Background()
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}

	// Keep one writer open so SQLite cannot checkpoint and remove the WAL
	// when NewSession closes its own short-lived connection.
	writer, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close writer: %v", err)
		}
	}()

	first, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	before := currentIndexRevision(repo)
	if err := writer.SetMeta("wal_revision_test", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("WAL write: %v", err)
	}
	after := currentIndexRevision(repo)
	if before == after {
		t.Fatal("committed WAL write did not change the observed index revision")
	}
	if before.database != after.database {
		t.Fatalf("test write unexpectedly changed main database revision: before=%+v after=%+v", before.database, after.database)
	}
	if before.wal == after.wal {
		t.Fatalf("test write did not change WAL revision: before=%+v after=%+v", before.wal, after.wal)
	}

	second, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	if second == first {
		t.Fatal("WAL-only index change must rebuild the pooled session")
	}
}

func TestSessionPoolRebuildsWhenSourceChangesWithoutIndexRevision(t *testing.T) {
	repo := newPoolTestRepo(t)
	ctx := context.Background()

	first, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	beforeRevision := currentIndexRevision(repo)

	sourcePath := filepath.Join(repo, "parser.go")
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	updated := []byte("package main\n\nfunc ParseRevision(input string) string {\n\treturn input\n}\n")
	if err := os.WriteFile(sourcePath, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	// Preserve both size and mtime so the regression proves that checksum
	// drift, rather than a cheap metadata hint, invalidates the pool.
	if err := os.Chtimes(sourcePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if afterEdit := currentIndexRevision(repo); afterEdit != beforeRevision {
		t.Fatalf("source-only edit changed index revision: before=%+v after=%+v", beforeRevision, afterEdit)
	}

	// Source checks are intentionally amortized: within the bounded TTL the
	// existing exact snapshot is reused without another full-tree checksum.
	withinTTL, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("session within source TTL: %v", err)
	}
	if withinTTL != first {
		t.Fatal("source generation was rechecked before its TTL expired")
	}

	// Expire the deterministic per-entry clock instead of sleeping so the
	// next lookup exercises the full checksum validation immediately.
	poolMu.Lock()
	first.sourceAt = time.Now().Add(-sourceGenerationTTL - time.Millisecond)
	poolMu.Unlock()

	second, err := sessionFor(ctx, repo)
	if err != nil {
		t.Fatalf("session after source edit: %v", err)
	}
	if second == first {
		t.Fatal("source checksum change must rebuild the pooled session")
	}
	response, err := second.session.Search(ctx, Options{
		Query: "ParseRevision",
		Limit: 3,
	})
	if err != nil {
		t.Fatalf("search rebuilt session: %v", err)
	}
	if len(response.Results) == 0 || response.Results[0].Symbol != "ParseRevision" {
		t.Fatalf("results = %+v, want reindexed ParseRevision on top", response.Results)
	}
}

func TestLoadStableSessionRetriesWhenRevisionChanges(t *testing.T) {
	revisionA := indexRevision{
		database: fileRevision{exists: true, size: 10, modTimeUnixNano: 1},
	}
	revisionB := indexRevision{
		database: fileRevision{exists: true, size: 20, modTimeUnixNano: 2},
	}
	revisions := []indexRevision{revisionA, revisionB, revisionB, revisionB}
	revisionReads := 0
	readRevision := func(string) indexRevision {
		if revisionReads >= len(revisions) {
			t.Fatalf("unexpected revision read %d", revisionReads+1)
		}
		revision := revisions[revisionReads]
		revisionReads++
		return revision
	}

	first := &Session{repo: "first"}
	second := &Session{repo: "second"}
	loads := 0
	load := func(context.Context, string) (*Session, error) {
		loads++
		if loads == 1 {
			return first, nil
		}
		return second, nil
	}

	got, revision, err := loadStableSessionWith(
		context.Background(),
		"/repo",
		load,
		readRevision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Fatalf("session = %p, want retried session %p", got, second)
	}
	if revision != revisionB {
		t.Fatalf("revision = %+v, want %+v", revision, revisionB)
	}
	if loads != 2 {
		t.Fatalf("session loads = %d, want 2", loads)
	}
}

func TestSessionPoolIsBounded(t *testing.T) {
	poolMu.Lock()
	originalPool := pool
	pool = make(map[string]*pooledSession)
	t.Cleanup(func() {
		poolMu.Lock()
		pool = originalPool
		poolMu.Unlock()
	})
	now := time.Now()
	for i := 0; i < maxPoolSessions+3; i++ {
		repo := filepath.Join("/tmp", "repo", string(rune('a'+i)))
		pool[repo] = &pooledSession{lastUsed: now.Add(time.Duration(i) * time.Second)}
		evictOldestSession(repo)
	}
	got := len(pool)
	poolMu.Unlock()

	if got != maxPoolSessions {
		t.Fatalf("pool size = %d, want %d", got, maxPoolSessions)
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
