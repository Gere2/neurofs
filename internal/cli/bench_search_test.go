package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/benchmark"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func TestRunSearchBenchmarkReportsHitsTokensAndStability(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	tmpDir := t.TempDir()

	code := `package service

func VerifyJWT() string {
	return "jwt verified"
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "auth.go"), []byte(code), 0o644); err != nil {
		t.Fatalf("write auth.go: %v", err)
	}

	cfg, err := config.New(tmpDir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	questions := []benchmark.Question{{
		Question:     "VerifyJWT",
		Expects:      []string{"auth.go"},
		ExpectsFacts: []string{"VerifyJWT", "jwt verified"},
	}}
	results, summary, err := runSearchBenchmark(context.Background(), tmpDir, questions, 3, 1, true)
	if err != nil {
		t.Fatalf("run search benchmark: %v", err)
	}
	if summary.Hits != 1 || summary.Top1 != 100 {
		t.Fatalf("expected perfect top-1 hit, got %+v", summary)
	}
	if summary.SearchMeanTokens <= 0 {
		t.Fatalf("expected positive search token count, got %+v", summary)
	}
	if summary.LatencyP50 <= 0 || summary.LatencyP95 <= 0 {
		t.Fatalf("expected positive search latency metrics, got %+v", summary)
	}
	if summary.FactQuestions != 1 || summary.FactRecall != 100 {
		t.Fatalf("expected perfect fact recall, got %+v", summary)
	}
	if !summary.StabilityChecked || summary.StablePrefixes != 1 || summary.Stability != 100 {
		t.Fatalf("expected stable search prefix, got %+v", summary)
	}
	if len(results) != 1 || results[0].MatchedAt != "auth.go" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestRunContextBenchmarkReportsRoutingHintsAndTokens(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	tmpDir := t.TempDir()

	code := `package service

func VerifyJWT() string {
	return "jwt verified"
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "auth.go"), []byte(code), 0o644); err != nil {
		t.Fatalf("write auth.go: %v", err)
	}

	cfg, err := config.New(tmpDir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	questions := []benchmark.Question{{
		Question: "VerifyJWT location",
		Expects:  []string{"auth.go"},
	}}
	results, summary, err := runContextBenchmark(context.Background(), tmpDir, questions, 3, 1)
	if err != nil {
		t.Fatalf("run context benchmark: %v", err)
	}
	if summary.Hits != 1 || summary.Top1 != 100 {
		t.Fatalf("expected perfect context top-1 hit, got %+v", summary)
	}
	if summary.ContextMeanTokens <= 0 || summary.LatencyP50 <= 0 {
		t.Fatalf("expected context token/latency metrics, got %+v", summary)
	}
	if summary.Routes["excerpt"] != 1 {
		t.Fatalf("expected excerpt route, got %+v", summary.Routes)
	}
	if summary.StructuralHints == 0 {
		t.Fatalf("expected structural hints, got %+v", summary)
	}
	if len(results) != 1 || results[0].MatchedAt != "auth.go" || results[0].Route != "excerpt" {
		t.Fatalf("unexpected context results: %+v", results)
	}
}
