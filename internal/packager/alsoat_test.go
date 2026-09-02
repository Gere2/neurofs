package packager

import (
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/models"
)

// TestPackChunksRendersAlsoAt covers the last mile of content dedupe: the
// folded-away paths have to reach the prompt, otherwise a reader looking for
// the copy they expected sees no trace of it.
func TestPackChunksRendersAlsoAt(t *testing.T) {
	bundle, err := PackChunks([]ChunkHit{{
		RelPath:       "output/build_poster.py",
		Lang:          models.LangPython,
		StartLine:     1,
		EndLine:       3,
		Kind:          "class",
		Symbol:        "Poster",
		Score:         42,
		TokenEstimate: 20,
		ContentHash:   "abc123",
		Snippet:       "class Poster:\n    def render(self):\n        return 1\n",
		AlsoAt:        []string{"tmp/pdfs/build_poster.py"},
	}}, "poster", Options{Budget: 2000})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if len(bundle.Fragments) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(bundle.Fragments))
	}
	content := bundle.Fragments[0].Content
	if !strings.Contains(content, "// also_at: tmp/pdfs/build_poster.py") {
		t.Fatalf("fragment does not cite the duplicate path:\n%s", content)
	}
	if !strings.Contains(content, "// file: output/build_poster.py") {
		t.Errorf("fragment lost its primary path:\n%s", content)
	}
}

func TestPackChunksOmitsAlsoAtWhenUnique(t *testing.T) {
	bundle, err := PackChunks([]ChunkHit{{
		RelPath:       "src/main.py",
		Lang:          models.LangPython,
		StartLine:     1,
		EndLine:       2,
		Kind:          "func",
		Symbol:        "main",
		Score:         10,
		TokenEstimate: 8,
		ContentHash:   "zzz",
		Snippet:       "def main():\n    return 0\n",
	}}, "main", Options{Budget: 2000})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if strings.Contains(bundle.Fragments[0].Content, "also_at") {
		t.Errorf("unique content must not carry an also_at line:\n%s", bundle.Fragments[0].Content)
	}
}
