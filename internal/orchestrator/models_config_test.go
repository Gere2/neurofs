package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultModelsConfig(t *testing.T) {
	cfg := DefaultModelsConfig()
	if len(cfg.Models) == 0 {
		t.Fatal("expected non-empty default models")
	}
	if _, ok := cfg.Models["claude-sonnet"]; !ok {
		t.Error("expected claude-sonnet in default models")
	}
	if _, ok := cfg.Models["gemini-flash"]; !ok {
		t.Error("expected gemini-flash in default models")
	}
	if _, ok := cfg.Models["gpt-4o-mini"]; !ok {
		t.Error("expected gpt-4o-mini in default models")
	}
	if cfg.Routing["frontend"] != "claude-sonnet" {
		t.Errorf("expected frontend route to be claude-sonnet, got %q", cfg.Routing["frontend"])
	}
}

func TestEstimateCost(t *testing.T) {
	entry := ModelEntry{
		CostInputPer1M:  3.0,
		CostOutputPer1M: 15.0,
	}
	cost := entry.EstimateCost(1000, 1000)
	expected := (1000.0/1_000_000.0)*3.0 + (1000.0/1_000_000.0)*15.0
	if cost != expected {
		t.Errorf("expected cost %f, got %f", expected, cost)
	}
}

func TestLoadModelsConfig_Fallback(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadModelsConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Error("expected fallback to default models config")
	}
}

func TestWriteAndLoadDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	neurofsDir := filepath.Join(dir, ".neurofs")
	if err := os.MkdirAll(neurofsDir, 0755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}

	if err := WriteDefaultConfig(neurofsDir); err != nil {
		t.Fatalf("failed to write default config: %v", err)
	}

	cfg, err := LoadModelsConfig(dir)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Error("expected loaded models, got empty")
	}
}
