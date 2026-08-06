package benchmark_test

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/benchmark"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/project"
	"github.com/Gere2/neurofs/internal/storage"
)

// TestSampleRepoPrecision locks in the ranking quality baseline on the
// sample repo. If this drops, something regressed; bump the threshold when
// a ranking change is intentional and improves numbers.
func TestSampleRepoPrecision(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "sample-repo")
	benchPath := filepath.Join(repoPath, ".neurofs-bench.json")

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "index.db")

	cfg := &config.Config{
		RepoRoot: repoPath,
		DBPath:   dbPath,
		Budget:   config.DefaultBudget,
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatalf("index: %v", err)
	}

	files, err := db.AllFiles()
	if err != nil {
		t.Fatalf("load files: %v", err)
	}

	questions, err := benchmark.LoadQuestions(benchPath)
	if err != nil {
		t.Fatalf("load benchmark: %v", err)
	}

	raw, _, _ := db.GetMeta(indexer.ProjectMetaKey)
	projInfo := project.Decode(raw)

	rels, _ := db.AllRelations()
	_, summary := benchmark.Run(files, questions, benchmark.RunOptions{
		TopK:      3,
		Project:   projInfo,
		Relations: rels,
	})

	const minTop3 = 75.0 // %
	if summary.Top3 < minTop3 {
		t.Errorf("top-3 precision regressed: got %.1f%%, want >= %.1f%%",
			summary.Top3, minTop3)
	}
	t.Logf("benchmark: %d/%d hits, top-1=%.1f%%, top-3=%.1f%%, top-5=%.1f%%, mean-rank=%.2f",
		summary.Hits, summary.Questions,
		summary.Top1, summary.Top3, summary.Top5, summary.MeanRank,
	)
}

// TestSampleRepoBundleSizes exercises the ComputeBundle path on the same
// sample repo. It does not assert exact token counts (those will drift with
// harmless content changes) — it only guarantees the metrics are populated
// and sane so the CI gate (--max-mean-bundle-tokens) has something to grade.
func TestSampleRepoBundleSizes(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "sample-repo")
	benchPath := filepath.Join(repoPath, ".neurofs-bench.json")

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "index.db")

	cfg := &config.Config{
		RepoRoot: repoPath,
		DBPath:   dbPath,
		Budget:   config.DefaultBudget,
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatalf("index: %v", err)
	}

	files, err := db.AllFiles()
	if err != nil {
		t.Fatalf("load files: %v", err)
	}

	questions, err := benchmark.LoadQuestions(benchPath)
	if err != nil {
		t.Fatalf("load benchmark: %v", err)
	}

	raw, _, _ := db.GetMeta(indexer.ProjectMetaKey)
	projInfo := project.Decode(raw)

	rels, _ := db.AllRelations()
	results, summary, err := benchmark.RunChecked(files, questions, benchmark.RunOptions{
		TopK:             3,
		Project:          projInfo,
		ComputeBundle:    true,
		PackBudget:       4000,
		PreferSignatures: true,
		Relations:        rels,
	})
	if err != nil {
		t.Fatalf("run bundle benchmark: %v", err)
	}

	if summary.BundleMeanTokens <= 0 {
		t.Fatalf("expected non-zero mean bundle tokens, got %d", summary.BundleMeanTokens)
	}
	if summary.BundleP50Tokens <= 0 || summary.BundleP95Tokens < summary.BundleP50Tokens {
		t.Fatalf("percentile ordering broken: p50=%d p95=%d", summary.BundleP50Tokens, summary.BundleP95Tokens)
	}
	if summary.BundleMeanTokens > 4000 {
		t.Fatalf("mean tokens %d exceeded packager budget 4000", summary.BundleMeanTokens)
	}
	for _, r := range results {
		if r.BundleTokens < 0 || r.BundleFiles < 0 {
			t.Fatalf("negative metric on %q: tokens=%d files=%d", r.Question, r.BundleTokens, r.BundleFiles)
		}
	}
	t.Logf("bundle sizes: mean=%d p50=%d p95=%d files=%d",
		summary.BundleMeanTokens, summary.BundleP50Tokens,
		summary.BundleP95Tokens, summary.BundleMeanFiles,
	)
}

func TestRunCheckedPropagatesBundleErrors(t *testing.T) {
	_, _, err := benchmark.RunChecked(
		nil,
		[]benchmark.Question{{Question: "invalid budget"}},
		benchmark.RunOptions{ComputeBundle: true, PackBudget: -1},
	)
	if err == nil || !strings.Contains(err.Error(), "budget must be positive") {
		t.Fatalf("RunChecked error = %v, want packager budget error", err)
	}
}

func TestRunRejectsBundleMode(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil || !strings.Contains(fmt.Sprint(got), "ComputeBundle requires RunChecked") {
			t.Fatalf("Run panic = %v, want checked-API guidance", got)
		}
	}()

	benchmark.Run(nil, nil, benchmark.RunOptions{ComputeBundle: true})
}
