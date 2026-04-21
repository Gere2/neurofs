package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndLoadRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := AuditRecord{
		Question:      "where is jwt verified?",
		Model:         "claude-manual",
		Timestamp:     time.Unix(1_700_000_000, 0).UTC(),
		BundleHash:    "abcdef0123456789",
		Response:      "see src/auth.ts",
		GroundedRatio: 1.0,
		Citations: []Citation{
			{Raw: "src/auth.ts", RelPath: "src/auth.ts", Valid: true},
		},
	}

	path, err := SaveRecord(dir, rec)
	if err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}
	if !strings.HasSuffix(path, ".json") {
		t.Fatalf("expected .json suffix, got %s", path)
	}
	if !strings.Contains(filepath.Base(path), "abcdef01") {
		t.Fatalf("expected short hash in filename, got %s", path)
	}

	loaded, err := LoadRecord(path)
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if loaded.Question != rec.Question || loaded.BundleHash != rec.BundleHash {
		t.Fatalf("round trip lost fields: %+v", loaded)
	}
	if len(loaded.Citations) != 1 || loaded.Citations[0].RelPath != "src/auth.ts" {
		t.Fatalf("citations not preserved: %+v", loaded.Citations)
	}
}

func TestListRecordsMissingDirIsNotError(t *testing.T) {
	paths, err := ListRecords(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if paths != nil {
		t.Fatalf("expected nil slice for missing dir, got %v", paths)
	}
}

func TestListRecordsSortsChronologically(t *testing.T) {
	dir := t.TempDir()
	// Write two records with different timestamps; newer should come second.
	older := AuditRecord{BundleHash: "aaaaaaaa11111111", Timestamp: time.Unix(1_700_000_000, 0)}
	newer := AuditRecord{BundleHash: "bbbbbbbb22222222", Timestamp: time.Unix(1_800_000_000, 0)}
	if _, err := SaveRecord(dir, older); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveRecord(dir, newer); err != nil {
		t.Fatal(err)
	}

	paths, err := ListRecords(dir)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
	if !strings.Contains(paths[0], "1700000000") || !strings.Contains(paths[1], "1800000000") {
		t.Fatalf("expected chronological order, got %v", paths)
	}
}

func TestSaveRecordDoesNotCollideOnSameBundleSameSecond(t *testing.T) {
	// Prior filename scheme was "<unix-sec>-<shorthash>.json" and happily
	// overwrote a record if you replayed the same bundle twice inside one
	// wall-clock second (which happens in practice: back-to-back UI
	// replays, CI parallel jobs). The new scheme appends a random suffix
	// so the same inputs always produce distinct filenames.
	dir := t.TempDir()
	rec := AuditRecord{
		BundleHash: "deadbeefcafebabe",
		Timestamp:  time.Unix(1_800_000_042, 0),
	}

	const runs = 32
	seen := make(map[string]bool, runs)
	for i := 0; i < runs; i++ {
		path, err := SaveRecord(dir, rec)
		if err != nil {
			t.Fatalf("SaveRecord(%d): %v", i, err)
		}
		if seen[path] {
			t.Fatalf("collision: path %q was produced twice in %d runs", path, i+1)
		}
		seen[path] = true
	}

	paths, err := ListRecords(dir)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(paths) != runs {
		t.Fatalf("expected %d persisted files, got %d", runs, len(paths))
	}
	// Every filename must still start with the unix-sec prefix so ordering
	// by lexical sort stays chronologically correct for mixed
	// legacy+new records.
	for _, p := range paths {
		if !strings.Contains(filepath.Base(p), "1800000042-deadbeef") {
			t.Errorf("expected prefix 1800000042-deadbeef in %s", p)
		}
	}
}

func TestListRecordsIgnoresNonJSON(t *testing.T) {
	dir := t.TempDir()
	rec := AuditRecord{BundleHash: "cafe0001cafe0001", Timestamp: time.Unix(1_700_000_001, 0)}
	if _, err := SaveRecord(dir, rec); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := ListRecords(dir)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected only the .json to be listed, got %v", paths)
	}
}
