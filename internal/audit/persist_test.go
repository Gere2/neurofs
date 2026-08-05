package audit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/fsutil"
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

// TestSaveAndLoadRecordPreservesParentRecord guards the lineage field:
// when set, ParentRecord must round-trip through disk; when unset, the
// loaded record must also have it unset (so legacy and from-scratch
// runs stay indistinguishable from the consumer's point of view).
func TestSaveAndLoadRecordPreservesParentRecord(t *testing.T) {
	dir := t.TempDir()
	parent := "1776696402-ddbb265c-abc123.json"
	rec := AuditRecord{
		Question:     "follow-up to the auth audit",
		BundleHash:   "deadbeef00112233",
		Timestamp:    time.Unix(1_700_000_100, 0).UTC(),
		ParentRecord: parent,
	}
	path, err := SaveRecord(dir, rec)
	if err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}
	loaded, err := LoadRecord(path)
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if loaded.ParentRecord != parent {
		t.Fatalf("ParentRecord lost in round-trip: got %q, want %q", loaded.ParentRecord, parent)
	}

	// Empty ParentRecord must stay empty after a round-trip — omitempty
	// drops the field on write and the zero value on read makes from-scratch
	// runs visually identical to legacy ones.
	rec2 := AuditRecord{
		Question:   "from-scratch run",
		BundleHash: "cafefacecafeface",
		Timestamp:  time.Unix(1_700_000_200, 0).UTC(),
	}
	path2, err := SaveRecord(dir, rec2)
	if err != nil {
		t.Fatalf("SaveRecord (no parent): %v", err)
	}
	loaded2, err := LoadRecord(path2)
	if err != nil {
		t.Fatalf("LoadRecord (no parent): %v", err)
	}
	if loaded2.ParentRecord != "" {
		t.Fatalf("ParentRecord should stay empty, got %q", loaded2.ParentRecord)
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

type failingInfoDirEntry struct {
	name string
	err  error
}

func (e failingInfoDirEntry) Name() string               { return e.name }
func (e failingInfoDirEntry) IsDir() bool                { return false }
func (e failingInfoDirEntry) Type() fs.FileMode          { return 0 }
func (e failingInfoDirEntry) Info() (fs.FileInfo, error) { return nil, e.err }

func TestListRecordsPropagatesEntryInfoError(t *testing.T) {
	dir := t.TempDir()
	sentinel := errors.New("entry metadata unavailable")
	_, err := listRecords(dir, func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{failingInfoDirEntry{name: "broken.json", err: sentinel}}, nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ListRecords entry error = %v, want wrapped sentinel", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "broken.json")) {
		t.Fatalf("ListRecords error does not identify entry: %v", err)
	}
}

func TestLoadRecordRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"question":"outside"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "record.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := LoadRecord(link); !errors.Is(err, fsutil.ErrNotRegular) {
		t.Fatalf("LoadRecord error = %v, want ErrNotRegular", err)
	}
}

func TestLoadRecordRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Truncate(maxAuditRecordBytes + 1); err != nil {
		_ = f.Close()
		t.Fatalf("Truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := LoadRecord(path); !errors.Is(err, fsutil.ErrFileTooLarge) {
		t.Fatalf("LoadRecord error = %v, want ErrFileTooLarge", err)
	}
}

func TestSaveRecordUsesPrivatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	path, err := SaveRecord(dir, AuditRecord{BundleHash: "private"})
	if err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat records dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("records dir permissions = %o, want 700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat record: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("record permissions = %o, want 600", got)
	}
}

func TestListRecordsIgnoresSymlinkJSON(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(`{"question":"outside"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	paths, err := ListRecords(dir)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("ListRecords returned symlink entries: %v", paths)
	}
}

func TestSaveAndListRejectSymlinkRecordsDir(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(parent, "records")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := SaveRecord(link, AuditRecord{BundleHash: "blocked"}); err == nil {
		t.Fatal("SaveRecord accepted a symlink records directory")
	}
	if _, err := ListRecords(link); err == nil {
		t.Fatal("ListRecords accepted a symlink records directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("SaveRecord wrote through symlink: %v", entries)
	}
}

func TestSaveRecordRejectsSymlinkParentDir(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "audit")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	dir := filepath.Join(repo, "audit", "records")
	if _, err := SaveRecord(dir, AuditRecord{BundleHash: "blocked"}); err == nil {
		t.Fatal("SaveRecord accepted a symlink parent directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "records")); !os.IsNotExist(err) {
		t.Fatalf("SaveRecord created records outside the repository: %v", err)
	}
}
