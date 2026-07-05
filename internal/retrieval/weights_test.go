package retrieval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadWeightsMissingFileReturnsDefaults(t *testing.T) {
	repo := t.TempDir()
	w, existed, err := LoadWeights(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if existed {
		t.Fatal("existed = true for missing file")
	}
	if w != DefaultWeights() {
		t.Fatalf("weights = %+v, want defaults", w)
	}
}

func TestSaveAndLoadWeightsRoundTrip(t *testing.T) {
	repo := t.TempDir()
	w := DefaultWeights()
	w.SymbolMatch = 12.5
	w.TestDownrank = 0.5
	if err := SaveWeights(repo, w); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, existed, err := LoadWeights(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !existed {
		t.Fatal("existed = false after save")
	}
	if got != w {
		t.Fatalf("round trip = %+v, want %+v", got, w)
	}
}

func TestLoadWeightsPartialFileKeepsDefaults(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".neurofs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A weights file written by an older binary that only knows one field.
	if err := os.WriteFile(WeightsPath(repo), []byte(`{"symbol_match": 4.0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadWeights(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SymbolMatch != 4.0 {
		t.Errorf("SymbolMatch = %v, want 4.0", got.SymbolMatch)
	}
	if got.PathMatch != DefaultWeights().PathMatch {
		t.Errorf("PathMatch = %v, want default %v", got.PathMatch, DefaultWeights().PathMatch)
	}
}

func TestLoadWeightsMalformedFallsBackToDefaults(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".neurofs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WeightsPath(repo), []byte(`{broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, existed, err := LoadWeights(repo)
	if err == nil {
		t.Fatal("want parse error")
	}
	if !existed {
		t.Fatal("existed = false for present malformed file")
	}
	if got != DefaultWeights() {
		t.Fatalf("weights = %+v, want defaults on malformed file", got)
	}
}

func TestClampBoundsTestDownrank(t *testing.T) {
	w := DefaultWeights()
	w.TestDownrank = 1.7 // above 1.0 a "penalty" would boost test files
	w.SymbolMatch = -3
	w.Clamp()
	if w.TestDownrank != 1.0 {
		t.Errorf("TestDownrank = %v, want clamp to 1.0", w.TestDownrank)
	}
	if w.SymbolMatch != 0 {
		t.Errorf("SymbolMatch = %v, want clamp to 0", w.SymbolMatch)
	}
}
