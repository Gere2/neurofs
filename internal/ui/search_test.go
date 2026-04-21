package ui

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/models"
)

// fixtureRecord returns a record with every searchable field populated so
// individual tests can filter down via scope instead of rebuilding.
func fixtureRecord() audit.AuditRecord {
	return audit.AuditRecord{
		Title:      "008 ui hardening",
		Brief:      "Harden the packer against symlink traversal.",
		Note:       "Done. Ship it after CI.",
		Question:   "explain how the ranker handles stemming",
		Model:      "claude-manual",
		Mode:       "build",
		BundleHash: "deadbeefcafebabe1111",
		Fragments: []audit.AuditFragment{
			{
				RelPath:        "internal/ranking/stem.go",
				Lang:           models.Lang("go"),
				Representation: models.Representation("signature"),
				Tokens:         55,
				Content:        "// file: stem.go\nfunc stemWord(w string) string { ... }\n",
			},
			{
				RelPath:        "internal/cli/ask.go",
				Lang:           models.Lang("go"),
				Representation: models.Representation("signature"),
				Content:        "package cli\n\nfunc ask() {}\n",
			},
		},
	}
}

func TestCollectSearchMatches_MetadataHit(t *testing.T) {
	rec := fixtureRecord()
	got := collectSearchMatches(rec, "hardening", "all")
	if len(got) != 1 || got[0].Field != "title" {
		t.Fatalf("expected single title match, got %+v", got)
	}
	if !strings.Contains(got[0].Snippet, "hardening") {
		t.Fatalf("snippet missing needle: %q", got[0].Snippet)
	}
}

func TestCollectSearchMatches_ScopePathsOnly(t *testing.T) {
	rec := fixtureRecord()
	// "ranking" appears both in fragment rel_path and in brief phrasing —
	// but brief doesn't have it, only path does. Verify scope=paths only
	// yields the fragment_path match.
	got := collectSearchMatches(rec, "ranking", "paths")
	if len(got) != 1 || got[0].Field != "fragment_path" {
		t.Fatalf("expected single path match, got %+v", got)
	}
	if got[0].RelPath != "internal/ranking/stem.go" {
		t.Fatalf("unexpected rel_path: %q", got[0].RelPath)
	}
}

func TestCollectSearchMatches_ScopeContentOnly(t *testing.T) {
	rec := fixtureRecord()
	// "stemWord" only appears inside the fragment content.
	got := collectSearchMatches(rec, "stemword", "content")
	if len(got) != 1 || got[0].Field != "fragment_content" {
		t.Fatalf("expected single content match, got %+v", got)
	}
}

func TestCollectSearchMatches_CaseInsensitive(t *testing.T) {
	rec := fixtureRecord()
	if len(collectSearchMatches(rec, "HARDENING", "metadata")) == 0 {
		t.Fatal("expected case-insensitive metadata match")
	}
	if len(collectSearchMatches(rec, "Stemword", "content")) == 0 {
		t.Fatal("expected case-insensitive content match")
	}
}

func TestCollectSearchMatches_EmptyFieldsAreSkipped(t *testing.T) {
	// A legacy-style record with no title/brief/note should still match
	// via its question without panicking or producing empty-field rows.
	rec := audit.AuditRecord{
		Question: "explain how the ranker handles stemming",
	}
	got := collectSearchMatches(rec, "ranker", "all")
	if len(got) != 1 || got[0].Field != "question" {
		t.Fatalf("expected lone question match, got %+v", got)
	}
}

func TestScoreMatches_WeightsByField(t *testing.T) {
	ms := []searchMatch{
		{Field: "title"},            // 3
		{Field: "fragment_path"},    // 2
		{Field: "fragment_content"}, // 1
	}
	if got := scoreMatches(ms); got != 6 {
		t.Fatalf("expected 3+2+1=6, got %d", got)
	}
}

func TestMatchesModeFilter(t *testing.T) {
	cases := []struct {
		recMode string
		filter  string
		want    bool
	}{
		{"build", "all", true},
		{"", "all", true},
		{"build", "build", true},
		{"build", "strategy", false},
		{"", "unknown", true},    // legacy records surface under "unknown"
		{"build", "unknown", false}, // and build records don't
	}
	for _, c := range cases {
		if got := matchesModeFilter(c.recMode, c.filter); got != c.want {
			t.Errorf("matchesModeFilter(%q, %q) = %v, want %v",
				c.recMode, c.filter, got, c.want)
		}
	}
}

func TestSnippetAround_CentersOnNeedleAndAddsEllipses(t *testing.T) {
	long := "prefix " + strings.Repeat("x", 60) + " TARGETED " + strings.Repeat("y", 60) + " suffix"
	got := snippetAround(long, "targeted", 40)
	if !strings.Contains(strings.ToLower(got), "targeted") {
		t.Fatalf("snippet lost the needle: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipses on both sides, got %q", got)
	}
}

func TestSnippetAround_UTF8Safe(t *testing.T) {
	// "café" is 5 bytes (c-a-f-[é=0xC3 0xA9]). Window that would otherwise
	// split the é must clamp to the rune boundary and not emit a
	// replacement char.
	haystack := "xxxxx café xxxxx"
	got := snippetAround(haystack, "café", 6)
	if !strings.Contains(got, "café") {
		t.Fatalf("lost the needle under tight window: %q", got)
	}
	// No replacement runes in the output.
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("snippet has replacement rune (rune boundary break): %q", got)
	}
}

func TestCollectSearchMatches_PerKindCap(t *testing.T) {
	// Build a record with 5 fragment paths containing the needle so we
	// can verify the per-kind cap stops collecting after 3.
	rec := audit.AuditRecord{}
	for i := 0; i < 5; i++ {
		rec.Fragments = append(rec.Fragments, audit.AuditFragment{
			RelPath: "internal/ranking/file" + string(rune('a'+i)) + ".go",
		})
	}
	got := collectSearchMatches(rec, "ranking", "paths")
	if len(got) != 3 {
		t.Fatalf("expected per-kind cap of 3 fragment_path hits, got %d", len(got))
	}
}
