package taskflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

// TestEnrichBundle_PopulatesIdentityFields verifies the audit-identity
// fields a compliance consumer needs are present after enrichment:
// resolved repo path, generation timestamp, and a content hash. CommitSHA
// is checked in TestEnrichBundle_CommitSHAFromGit; here we tolerate an
// empty SHA because the test tempdir is not a git worktree.
func TestEnrichBundle_PopulatesIdentityFields(t *testing.T) {
	repo := t.TempDir()
	before := time.Now().UTC().Add(-time.Second)

	b := EnrichBundle(models.Bundle{
		Query:     "q",
		Fragments: []models.ContextFragment{{RelPath: "a.go", Content: "x"}},
	}, repo)

	if b.Repo == "" || !filepath.IsAbs(b.Repo) {
		t.Errorf("Repo must be absolute; got %q", b.Repo)
	}
	if b.GeneratedAt.Before(before) {
		t.Errorf("GeneratedAt must be set to now; got %v", b.GeneratedAt)
	}
	if len(b.BundleHash) != 64 {
		t.Errorf("BundleHash must be sha256-hex (64 chars); got %d chars", len(b.BundleHash))
	}
}

// TestEnrichBundle_HashStableAcrossEnrichmentRuns confirms BundleHash
// excludes GeneratedAt — otherwise two enrich runs with identical
// content would produce different hashes, defeating the "same context"
// guarantee.
func TestEnrichBundle_HashStableAcrossEnrichmentRuns(t *testing.T) {
	repo := t.TempDir()
	b := models.Bundle{
		Query:     "q",
		Fragments: []models.ContextFragment{{RelPath: "a.go", Content: "x"}},
	}
	h1 := EnrichBundle(b, repo).BundleHash
	time.Sleep(10 * time.Millisecond)
	h2 := EnrichBundle(b, repo).BundleHash
	if h1 != h2 {
		t.Errorf("BundleHash must NOT depend on GeneratedAt; got %s vs %s", h1, h2)
	}
}

// TestEnrichBundle_CommitSHAFromGit confirms we capture the HEAD commit
// when the repo is a git worktree.
func TestEnrichBundle_CommitSHAFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	b := EnrichBundle(models.Bundle{Query: "q"}, repo)
	if len(b.CommitSHA) != 40 {
		t.Errorf("CommitSHA must be a 40-char hex SHA from git rev-parse; got %q", b.CommitSHA)
	}
}

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

func TestRunUsesChunkMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")

	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/chunktest\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	source := `package chunktest

func BuildThing(name string) string {
	return "build:" + name
}

func OtherThing(name string) string {
	return "other:" + name
}
`
	if err := os.WriteFile(filepath.Join(tmp, "builder.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	result, err := Run(Opts{
		RepoRoot:      tmp,
		Query:         "Where is BuildThing implemented?",
		Budget:        1200,
		Force:         true,
		DisableChunks: false,
	})
	if err != nil {
		t.Fatalf("Run chunk mode failed: %v", err)
	}
	if !result.ChunkMode {
		t.Fatalf("expected ChunkMode=true")
	}
	if len(result.Bundle.Fragments) == 0 {
		t.Fatalf("expected chunk fragments")
	}
	frag := result.Bundle.Fragments[0]
	if frag.Representation != models.RepExcerpt {
		t.Fatalf("expected excerpt fragment, got %q", frag.Representation)
	}
	if !strings.Contains(frag.Content, "// lines:") || !strings.Contains(frag.Content, "BuildThing") {
		t.Fatalf("fragment does not look like a chunk excerpt:\n%s", frag.Content)
	}
	if !strings.Contains(result.Prompt, `rep="excerpt"`) || !strings.Contains(result.Prompt, "// lines:") {
		t.Fatalf("prompt missing excerpt metadata:\n%s", result.Prompt)
	}
	if !strings.Contains(filepath.Base(result.PromptPath), "chunks-") {
		t.Fatalf("chunk cache should use a distinct filename, got %s", result.PromptPath)
	}
}

func TestGitDiffAndStatus(t *testing.T) {
	tmp := t.TempDir()
	diff := GitDiff(tmp)
	if diff != "" {
		t.Errorf("expected empty diff on non-git dir, got: %q", diff)
	}

	status := GitStatus(tmp)
	if status != "" {
		t.Errorf("expected empty status on non-git dir, got: %q", status)
	}
}
