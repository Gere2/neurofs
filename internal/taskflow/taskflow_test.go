package taskflow

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

// TestTopPicks pins the structured shape of the top-N selection that
// both the CLI summary and the UI panel render. If fields move or the
// ordering contract changes, the UX affordance ("see what landed
// without opening the prompt") regresses silently.
func TestTopPicks(t *testing.T) {
	t.Parallel()

	b := models.Bundle{
		Fragments: []models.ContextFragment{
			{RelPath: "internal/ranking/ranking.go", Tokens: 820, Representation: models.Representation("full_code"), Score: 9.1},
			{RelPath: "internal/packager/packager.go", Tokens: 410, Representation: models.Representation("signature"), Score: 6.4},
			{RelPath: "cmd/neurofs/main.go", Tokens: 90, Representation: models.Representation("full_code"), Score: 2.1},
		},
	}

	t.Run("respects n and fragment order", func(t *testing.T) {
		got := TopPicks(b, 2)
		if len(got) != 2 {
			t.Fatalf("want 2 picks, got %d: %+v", len(got), got)
		}
		if got[0].RelPath != "internal/ranking/ranking.go" {
			t.Fatalf("first pick wrong: %+v", got[0])
		}
		if got[0].Tokens != 820 || got[0].Representation != "full_code" || got[0].Score != 9.1 {
			t.Fatalf("first pick fields wrong: %+v", got[0])
		}
	})

	t.Run("caps at fragment count", func(t *testing.T) {
		got := TopPicks(b, 99)
		if len(got) != 3 {
			t.Fatalf("want 3 picks, got %d", len(got))
		}
	})

	t.Run("nil on empty or zero", func(t *testing.T) {
		if got := TopPicks(b, 0); got != nil {
			t.Fatalf("want nil for n=0, got %v", got)
		}
		if got := TopPicks(models.Bundle{}, 5); got != nil {
			t.Fatalf("want nil for empty bundle, got %v", got)
		}
	})
}

// TestBaseName guarantees cache-key determinism: same inputs → same
// filename; a different budget must yield a different filename.
func TestBaseName(t *testing.T) {
	t.Parallel()

	a := BaseName("implement resume from record", 8000)
	b := BaseName("implement resume from record", 8000)
	if a != b {
		t.Fatalf("same inputs must produce same base name: %q vs %q", a, b)
	}
	c := BaseName("implement resume from record", 3000)
	if a == c {
		t.Fatalf("different budget must produce different base: both %q", a)
	}
	if len(a) < 10 || a[8] != '-' {
		t.Fatalf("base name shape wrong: %q", a)
	}
}

// TestSlugify covers lowercase, non-alnum collapse, the 40-char cap
// with hyphen trim, and the empty-string fallback. The fallback case
// matters — a slug that collapses to "" would mean every query
// shared one cache slot.
func TestSlugify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"Hello World!", "hello-world"},
		{"  leading/trailing  ", "leading-trailing"},
		{"multi   space --- dashes", "multi-space-dashes"},
		{"", "task"},
		{"!!!", "task"},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
	}
	for _, tc := range cases {
		if got := Slugify(tc.in); got != tc.want {
			t.Fatalf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
