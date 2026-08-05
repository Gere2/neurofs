package cli

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/gate"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

type gateIndexArtifactSnapshot struct {
	Exists bool
	Mode   os.FileMode
	Size   int64
	MTime  time.Time
	SHA256 [sha256.Size]byte
}

func TestRunFixturesKeepsStaleIndexReadOnly(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	sourcePath := filepath.Join(repo, "answer.go")
	if err := os.WriteFile(sourcePath, []byte(`package answer

const IndexedSentinel = "indexed_fact"
`), 0o644); err != nil {
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
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	indexedCount, err := db.FileCount()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Exercise both implicit refresh triggers: the index itself is old and the
	// working tree contains a new indexable source file.
	staleTime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(cfg.DBPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repo, "added.go"),
		[]byte("package answer\n\nconst AddedAfterScan = true\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if issue := gateIndexFreshnessIssue(cfg); !strings.Contains(issue, "source generation") {
		t.Fatalf("new indexable source freshness issue = %q, want source-generation warning", issue)
	}

	before := snapshotGateIndexArtifacts(t, cfg.DBPath)
	results, snapshots, attestations := runFixtures(context.Background(), repo, []gate.Fixture{{
		Question:     "Where is IndexedSentinel defined?",
		ExpectsFacts: []string{"IndexedSentinel"},
	}}, 1200, false)
	if len(results) != 1 {
		t.Fatalf("runFixtures returned %d results, want 1", len(results))
	}
	if results[0].Error != "" {
		t.Fatalf("fixture failed: %s", results[0].Error)
	}
	if results[0].Recall != 1 {
		t.Fatalf("fixture recall = %.2f, want 1.0", results[0].Recall)
	}
	if len(snapshots) != 1 || len(attestations) != 1 {
		t.Fatalf(
			"fixture bundle evidence = %d snapshots/%d attestations, want 1/1",
			len(snapshots), len(attestations),
		)
	}

	after := snapshotGateIndexArtifacts(t, cfg.DBPath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("gate changed index artifacts:\nbefore: %#v\nafter:  %#v", before, after)
	}

	readOnlyDB, err := storage.OpenReadOnly(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnlyDB.Close() }()
	files, err := readOnlyDB.AllFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != indexedCount {
		t.Fatalf("indexed file count changed from %d to %d", indexedCount, len(files))
	}
	for _, file := range files {
		if file.RelPath == "added.go" {
			t.Fatal("gate implicitly indexed source added after the measured scan")
		}
	}

	cacheEntries, err := os.ReadDir(filepath.Join(repo, config.DirName, "task"))
	if err != nil {
		t.Fatalf("gate did not write its documented task cache: %v", err)
	}
	if len(cacheEntries) != 3 {
		t.Fatalf("task cache contains %d entries, want prompt, bundle, and manifest", len(cacheEntries))
	}
	for _, entry := range cacheEntries {
		if entry.IsDir() ||
			(!strings.HasSuffix(entry.Name(), ".prompt.txt") &&
				!strings.HasSuffix(entry.Name(), ".bundle.json") &&
				!strings.HasSuffix(entry.Name(), ".manifest.json")) {
			t.Fatalf("unexpected task cache artifact %q", entry.Name())
		}
	}
}

func snapshotGateIndexArtifacts(t *testing.T, dbPath string) map[string]gateIndexArtifactSnapshot {
	t.Helper()
	snapshots := make(map[string]gateIndexArtifactSnapshot, 3)
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			snapshots[path] = gateIndexArtifactSnapshot{}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[path] = gateIndexArtifactSnapshot{
			Exists: true,
			Mode:   info.Mode(),
			Size:   info.Size(),
			MTime:  info.ModTime(),
			SHA256: sha256.Sum256(data),
		}
	}
	return snapshots
}
