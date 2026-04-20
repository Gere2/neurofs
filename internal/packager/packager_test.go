package packager_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/packager"
)

// writeTempFile writes the given content under t.TempDir() and returns its
// absolute path. Used by tests to feed real files through packager.Pack,
// since the packager reads file contents from disk.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func sampleRanked(t *testing.T) []models.ScoredFile {
	t.Helper()
	mk := func(name, content string, score float64) models.ScoredFile {
		p := writeTempFile(t, name, content)
		return models.ScoredFile{
			Record: models.FileRecord{
				Path:    p,
				RelPath: "src/" + name,
				Lang:    models.LangTypeScript,
			},
			Score: score,
		}
	}
	return []models.ScoredFile{
		mk("auth.ts", "export const AUTH = 1;", 5.0),
		mk("crypto.ts", "export const CRYPTO = 2;", 4.0),
		mk("user.ts", "export const USER = 3;", 3.0),
		mk("api.ts", "export const API = 4;", 2.0),
	}
}

func TestPackMaxFilesCap(t *testing.T) {
	ranked := sampleRanked(t)
	b, err := packager.Pack(ranked, "q", packager.Options{Budget: 4000, MaxFiles: 2})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if b.Stats.FilesIncluded != 2 {
		t.Errorf("MaxFiles=2 should cap at 2, got %d", b.Stats.FilesIncluded)
	}
	if b.Fragments[0].RelPath != "src/auth.ts" {
		t.Errorf("highest-scoring file should come first, got %s", b.Fragments[0].RelPath)
	}
}

func TestPackMaxFragmentsCap(t *testing.T) {
	ranked := sampleRanked(t)
	b, _ := packager.Pack(ranked, "q", packager.Options{Budget: 4000, MaxFragments: 1})
	if len(b.Fragments) != 1 {
		t.Errorf("MaxFragments=1 should produce 1 fragment, got %d", len(b.Fragments))
	}
}

func TestPackPreferSignaturesForLargeFile(t *testing.T) {
	// Build a file that is slightly larger than aggressiveFullCodeMaxTokens
	// but small enough to fit under the normal threshold. With
	// PreferSignatures=true it should collapse to a signature or
	// structural_note rather than full_code.
	big := ""
	for i := 0; i < 300; i++ {
		big += "// pad line just to blow past the aggressive full-code cap\n"
	}
	ranked := []models.ScoredFile{{
		Record: models.FileRecord{
			Path:    writeTempFile(t, "big.ts", big),
			RelPath: "src/big.ts",
			Lang:    models.LangTypeScript,
		},
		Score: 5.0,
	}}
	b, _ := packager.Pack(ranked, "q", packager.Options{Budget: 4000, PreferSignatures: true})
	if len(b.Fragments) == 0 {
		t.Fatalf("expected at least one fragment")
	}
	if b.Fragments[0].Representation == models.RepFullCode {
		t.Errorf("PreferSignatures should avoid full_code here, got %s", b.Fragments[0].Representation)
	}
}
