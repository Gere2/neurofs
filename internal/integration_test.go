// Package neurofs_test runs an end-to-end integration test against the
// sample repository in testdata/sample-repo.
package neurofs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func TestEndToEnd(t *testing.T) {
	// Locate testdata relative to this file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoPath := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "sample-repo")

	// Use a temp dir for the database so tests don't pollute the source tree.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "index.db")

	cfg := &config.Config{
		RepoRoot: repoPath,
		DBPath:   dbPath,
		Budget:   config.DefaultBudget,
	}

	// ─── Phase 1: index ──────────────────────────────────────────────────────
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	stats, err := indexer.Run(cfg, db, indexer.Options{})
	if err != nil {
		t.Fatalf("indexer.Run: %v", err)
	}

	if stats.Indexed == 0 {
		t.Fatal("indexer found no files — check testdata/sample-repo")
	}
	t.Logf("indexed %d files, %d symbols, %d imports",
		stats.Indexed, stats.Symbols, stats.Imports)

	// ─── Phase 2: rank ───────────────────────────────────────────────────────
	files, err := db.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}

	ranked := ranking.Rank(files, "authentication login jwt token")

	if len(ranked) == 0 {
		t.Fatal("ranking returned no results")
	}

	// auth.ts must rank highly for an auth query.
	topPath := ranked[0].Record.RelPath
	if !strings.Contains(topPath, "auth") {
		t.Errorf("expected auth file at top, got %s (score=%.2f)", topPath, ranked[0].Score)
	}
	if ranked[0].Score <= 0 {
		t.Errorf("top file has zero score")
	}

	t.Logf("top result: %s (score=%.2f, reasons=%d)",
		topPath, ranked[0].Score, len(ranked[0].Reasons))

	// ─── Phase 3: pack ───────────────────────────────────────────────────────
	bundle, err := packager.Pack(ranked, "authentication login jwt token",
		packager.Options{Budget: 4000})
	if err != nil {
		t.Fatalf("packager.Pack: %v", err)
	}

	if bundle.Stats.FilesIncluded == 0 {
		t.Fatal("bundle contains no fragments")
	}
	if bundle.Stats.TokensUsed > bundle.Stats.TokensBudget {
		t.Errorf("bundle exceeds budget: %d > %d",
			bundle.Stats.TokensUsed, bundle.Stats.TokensBudget)
	}

	t.Logf("bundle: %d files, %d/%d tokens, %.1fx compression",
		bundle.Stats.FilesIncluded,
		bundle.Stats.TokensUsed,
		bundle.Stats.TokensBudget,
		bundle.Stats.CompressionRatio,
	)

	// Every fragment must have at least one inclusion reason.
	for _, frag := range bundle.Fragments {
		if len(frag.Reasons) == 0 {
			t.Errorf("fragment %s has no inclusion reasons", frag.RelPath)
		}
		if frag.Tokens <= 0 {
			t.Errorf("fragment %s has zero tokens", frag.RelPath)
		}
	}

	// ─── Phase 4: output ─────────────────────────────────────────────────────
	var sb strings.Builder
	if err := output.Write(&sb, bundle, output.FormatMarkdown); err != nil {
		t.Fatalf("output.Write: %v", err)
	}

	md := sb.String()
	if !strings.Contains(md, "NeuroFS Context Bundle") {
		t.Error("markdown output missing bundle header")
	}
	if !strings.Contains(md, "authentication login jwt token") {
		t.Error("markdown output missing query")
	}

	t.Logf("output length: %d bytes", len(md))

	// ─── Phase 5: pack to file ───────────────────────────────────────────────
	outFile := filepath.Join(tmpDir, "bundle.prompt")
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatalf("create output file: %v", err)
	}
	defer f.Close()

	if err := output.Write(f, bundle, output.FormatMarkdown); err != nil {
		t.Fatalf("write to file: %v", err)
	}

	info, err := os.Stat(outFile)
	if err != nil {
		t.Fatalf("stat output file: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output file is empty")
	}
	t.Logf("bundle file: %s (%d bytes)", outFile, info.Size())
}
