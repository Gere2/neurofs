package gate

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestMarkStaleFactsFlagsOnlyDeadIdentifiers(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed — MarkStaleFacts degrades to a no-op without it")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"),
		[]byte("package main\n\nfunc realFact() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := []FactResult{
		{Misses: []string{"realFact", "identifierDeletedInRefactor"}},
		{Misses: nil},
	}
	MarkStaleFacts(repo, results)

	// realFact exists in the repo: a retrieval gap, not a stale fixture.
	if len(results[0].StaleFacts) != 1 || results[0].StaleFacts[0] != "identifierDeletedInRefactor" {
		t.Fatalf("StaleFacts = %v, want only the dead identifier", results[0].StaleFacts)
	}
	if results[1].StaleFacts != nil {
		t.Fatalf("no misses must mean no stale facts, got %v", results[1].StaleFacts)
	}
}

func TestCountStaleIndexFiles(t *testing.T) {
	repo := t.TempDir()
	fresh := []byte("package main\n")
	if err := os.WriteFile(filepath.Join(repo, "fresh.go"), fresh, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "changed.go"), []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := []models.FileRecord{
		{RelPath: "fresh.go", Checksum: fmt.Sprintf("%x", sha256.Sum256(fresh))},
		{RelPath: "changed.go", Checksum: "checksum-of-the-old-content"},
		{RelPath: "deleted.go", Checksum: "whatever"},
	}
	if got := CountStaleIndexFiles(repo, files); got != 2 {
		t.Fatalf("stale count = %d, want 2 (changed + deleted)", got)
	}
}

func TestFormatStaleFacts(t *testing.T) {
	if got := formatStaleFacts(nil); got != "" {
		t.Fatalf("empty stale facts must render nothing, got %q", got)
	}
	got := formatStaleFacts([]string{"weightFilename"})
	want := " [weightFilename no longer in repo — stale fixture?]"
	if got != want {
		t.Fatalf("formatStaleFacts = %q, want %q", got, want)
	}
}
