package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/models"
)

// Two staleness diagnostics, both born from a real hour of confusion
// (2026-07-04): a refactor deleted an identifier that two G3 fixtures
// expected, and the index still described the pre-refactor tree. G3
// reported generic misses; the operator had to hand-derive both causes.
// These helpers name them directly in the report.

// MarkStaleFacts annotates each result's misses that no longer occur
// anywhere in the repo — a missing fact that exists somewhere is a
// retrieval/packing gap, but a missing fact that exists NOWHERE is a
// rotten fixture (the code moved; the oracle didn't). Uses ripgrep like
// the promote guard; without rg installed nothing is annotated.
func MarkStaleFacts(repoRoot string, results []FactResult) {
	if _, err := exec.LookPath("rg"); err != nil {
		return
	}
	for i := range results {
		for _, miss := range results[i].Misses {
			if !factExistsInRepo(repoRoot, miss) {
				results[i].StaleFacts = append(results[i].StaleFacts, miss)
			}
		}
	}
}

func factExistsInRepo(repoRoot, fact string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rg", "--fixed-strings", "--ignore-case", "--quiet",
		"--glob", "!.git/**", "--glob", "!.neurofs/**", "--glob", "!audit/facts/**", fact, repoRoot)
	return cmd.Run() == nil
}

// CountStaleIndexFiles reports how many indexed files no longer match the
// working tree (content changed or file deleted since the last scan). G3
// and G4 ground against the index; running them over a stale index
// produces misaligned excerpts and phantom misses, so the gate warns
// before it confuses anyone.
func CountStaleIndexFiles(repoRoot string, files []models.FileRecord) int {
	stale := 0
	for _, f := range files {
		abs, err := fsutil.ConfineToRepoStrict(repoRoot, f.RelPath)
		if err != nil {
			stale++
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			stale++
			continue
		}
		if fmt.Sprintf("%x", sha256.Sum256(content)) != f.Checksum {
			stale++
		}
	}
	return stale
}
