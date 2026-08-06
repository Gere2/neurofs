package ranking

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRankingWeightsSaveLoadRoundTripSecurely(t *testing.T) {
	repo := t.TempDir()
	want := DefaultWeights()
	want.Symbol = 17.5
	if err := SaveWeights(repo, want); err != nil {
		t.Fatalf("SaveWeights: %v", err)
	}

	got, existed, err := LoadWeights(repo)
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}
	if !existed {
		t.Fatal("LoadWeights reported a missing file after SaveWeights")
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	info, err := os.Lstat(WeightsPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("weights mode = %v (%o), want regular 600", info.Mode(), info.Mode().Perm())
	}
}

func TestRankingWeightsRejectUnsafeReadsAndReplaceSymlink(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(WeightsPath(repo)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(WeightsPath(repo), make([]byte, maxWeightsFileSize+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadWeights(repo); err == nil {
			t.Fatal("oversized ranking weights file was loaded")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(WeightsPath(repo)), 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.json")
		const sentinel = `{"symbol":59}`
		if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, WeightsPath(repo)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, _, err := LoadWeights(repo); err == nil {
			t.Fatal("symlinked ranking weights file was loaded")
		}

		want := DefaultWeights()
		want.Symbol = 11
		if err := SaveWeights(repo, want); err != nil {
			t.Fatalf("replace symlink atomically: %v", err)
		}
		info, err := os.Lstat(WeightsPath(repo))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			t.Fatalf("weights path was not replaced by a regular file: %v", info.Mode())
		}
		outsideData, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if string(outsideData) != sentinel {
			t.Fatalf("symlink target changed: %q", outsideData)
		}
		got, _, err := LoadWeights(repo)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("loaded weights = %+v, want %+v", got, want)
		}
	})
}
