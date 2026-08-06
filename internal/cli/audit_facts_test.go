package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/models"
)

func TestParseFactsMergesCSVAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "facts.txt")
	if err := os.WriteFile(path, []byte("from file\n\n second file fact \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseFacts("csv fact, another", path)
	if err != nil {
		t.Fatalf("parseFacts: %v", err)
	}
	want := []string{"csv fact", "another", "from file", "second file fact"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("facts = %#v, want %#v", got, want)
	}
}

func TestParseFactsReportsMissingFile(t *testing.T) {
	_, err := parseFacts("", filepath.Join(t.TempDir(), "missing.txt"))
	if err == nil {
		t.Fatal("explicit --facts-file must not fail open")
	}
}

func TestAuditSummariesPropagateWriterErrors(t *testing.T) {
	writeErr := errors.New("write failed")
	writer := errorWriter{err: writeErr}

	if err := printDiffSummary(writer, audit.Diff{}, "a.json", "b.json"); !errors.Is(err, writeErr) {
		t.Fatalf("printDiffSummary error = %v, want %v", err, writeErr)
	}
	if err := printReplaySummary(writer, audit.AuditRecord{}); !errors.Is(err, writeErr) {
		t.Fatalf("printReplaySummary error = %v, want %v", err, writeErr)
	}
}

func TestAuditInputsRejectSymlinksAndOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	oversizedFacts := filepath.Join(dir, "facts.txt")
	if err := os.WriteFile(oversizedFacts, []byte(strings.Repeat("x", int(maxAuditFactsBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFacts("", oversizedFacts); err == nil {
		t.Fatal("oversized facts file was accepted")
	}

	outside := filepath.Join(dir, "outside.bundle.json")
	link := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(outside, []byte(`{"fragments":[{"rel_path":"secret.go"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := loadBundleJSON(link); err == nil {
		t.Fatal("symlinked audit bundle was loaded")
	}

	snapshot := filepath.Join(dir, "snapshot.json")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(snapshot, models.Bundle{Query: "safe"}); err != nil {
		t.Fatalf("atomic snapshot replacement: %v", err)
	}
	info, err := os.Lstat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("writeJSON left a symlink at the snapshot path")
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(outsideData) != `{"fragments":[{"rel_path":"secret.go"}]}` {
		t.Fatal("writeJSON modified the symlink target")
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}
