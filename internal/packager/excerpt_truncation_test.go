package packager

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

// TestRenderExcerptTruncatesOversizedBlocks verifies the headline
// behaviour: a block taller than excerptBlockMaxLines is rendered as
// {head} + elision marker + {tail}, with the marker carrying the
// exact count of elided lines.
func TestRenderExcerptTruncatesOversizedBlocks(t *testing.T) {
	const totalLines = 100
	lines := make([]string, totalLines)
	for i := range lines {
		lines[i] = fmt.Sprintf("    // body line %03d", i+1)
	}
	b := block{startLine: 1, endLine: totalLines, symbol: "func giant"}

	out := renderExcerptWithOptions(
		models.FileRecord{RelPath: "x.go", Lang: models.LangGo},
		lines,
		[]block{b},
		renderOptions{truncateBlocksOver: excerptBlockMaxLines},
	)

	wantElided := totalLines - excerptBlockHeadLines - excerptBlockTailLines
	wantMarker := fmt.Sprintf("// ... %d lines elided in extracted excerpt ...", wantElided)
	if !strings.Contains(out, wantMarker) {
		t.Fatalf("expected elision marker %q in output:\n%s", wantMarker, out)
	}

	// Head: lines 1..excerptBlockHeadLines must be present.
	for i := 1; i <= excerptBlockHeadLines; i++ {
		needle := fmt.Sprintf("body line %03d", i)
		if !strings.Contains(out, needle) {
			t.Errorf("head line %d missing: %q", i, needle)
		}
	}
	// Tail: last excerptBlockTailLines lines must be present.
	for i := totalLines - excerptBlockTailLines + 1; i <= totalLines; i++ {
		needle := fmt.Sprintf("body line %03d", i)
		if !strings.Contains(out, needle) {
			t.Errorf("tail line %d missing: %q", i, needle)
		}
	}
	// A line in the middle must be ABSENT.
	mid := totalLines / 2
	if strings.Contains(out, fmt.Sprintf("body line %03d", mid)) {
		t.Errorf("middle line %d should have been elided", mid)
	}
}

// TestRenderExcerptKeepsSmallBlocksIntact confirms that the truncation
// is opt-in by size: a block at the threshold or below stays intact, no
// elision marker is added.
func TestRenderExcerptKeepsSmallBlocksIntact(t *testing.T) {
	for _, span := range []int{1, 10, excerptBlockMaxLines} {
		t.Run(fmt.Sprintf("span=%d", span), func(t *testing.T) {
			lines := make([]string, span)
			for i := range lines {
				lines[i] = fmt.Sprintf("body %d", i+1)
			}
			b := block{startLine: 1, endLine: span, symbol: "func small"}
			out := renderExcerptWithOptions(
				models.FileRecord{RelPath: "y.go", Lang: models.LangGo},
				lines,
				[]block{b},
				renderOptions{truncateBlocksOver: excerptBlockMaxLines},
			)
			if strings.Contains(out, "elided in extracted excerpt") {
				t.Errorf("span=%d block was unexpectedly truncated:\n%s", span, out)
			}
			// Every body line must be present.
			for i := 1; i <= span; i++ {
				if !strings.Contains(out, fmt.Sprintf("body %d", i)) {
					t.Errorf("missing line body %d in non-truncated render", i)
				}
			}
		})
	}
}

// TestRenderExcerptDefaultDoesNotTruncate guards that the no-options
// path used by TS/JS/Python keeps emitting the full body — the
// pre-existing semantics callers depend on.
func TestRenderExcerptDefaultDoesNotTruncate(t *testing.T) {
	const span = 200
	lines := make([]string, span)
	for i := range lines {
		lines[i] = fmt.Sprintf("    line %d", i+1)
	}
	b := block{startLine: 1, endLine: span, symbol: "func huge"}
	out := renderExcerpt(
		models.FileRecord{RelPath: "ts.ts", Lang: models.LangTypeScript},
		lines,
		[]block{b},
	)
	if strings.Contains(out, "elided in extracted excerpt") {
		t.Errorf("default render must not truncate; got marker in output")
	}
	if !strings.Contains(out, fmt.Sprintf("line %d", span/2)) {
		t.Errorf("default render dropped a middle line")
	}
}

// TestExtractGoExcerptDoesNotMergeAdjacentDecls is the critical Go-path
// regression: two top-level Go declarations separated by a blank line
// (`type ... }` then blank then `func ... {`) must NOT collapse into
// one block. Before mergeOverlappingStrict, the gap-tolerance merge
// fused them into a single 130+ line block that blew through the cap.
func TestExtractGoExcerptDoesNotMergeAdjacentDecls(t *testing.T) {
	content := `package foo

// Options configures the thing.
type Options struct {
	UpgradeWithSlack bool
}

func Pack() string {
	return "unrelated body"
}
`
	rec := models.FileRecord{RelPath: "foo/foo.go", Lang: models.LangGo}
	out, ok := extractGoExcerpt(rec, content, []string{"upgrade", "slack"})
	if !ok {
		t.Fatalf("expected excerpt for Options.UpgradeWithSlack field")
	}
	// "func Pack" body has no matching symbol; with strict merge it
	// must NOT appear in the excerpt because it is in a separate block
	// that did not match.
	if strings.Contains(out, `return "unrelated body"`) {
		t.Errorf("strict merge violated: unrelated func Pack body present:\n%s", out)
	}
	if !strings.Contains(out, "UpgradeWithSlack bool") {
		t.Errorf("matched field missing from excerpt:\n%s", out)
	}
}

// TestExtractGoExcerptTruncationRescuesLongFunction is the end-to-end
// proof: a Go file containing one matched but oversized function
// produces an excerpt whose token cost stays under excerptMaxTokens.
// Before the change, this exact shape produced a 1200+ token excerpt
// that the packager rejected and replaced with a signature.
func TestExtractGoExcerptTruncationRescuesLongFunction(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package big\n\n")
	sb.WriteString("// configureRouter wires the HTTP routes.\n")
	sb.WriteString("func configureRouter() error {\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "\trouter.Handle(\"/path%03d\", handler%03d)\n", i, i)
	}
	sb.WriteString("\treturn nil\n")
	sb.WriteString("}\n")

	rec := models.FileRecord{RelPath: "router/router.go", Lang: models.LangGo}
	out, ok := extractGoExcerpt(rec, sb.String(), []string{"router", "configure"})
	if !ok {
		t.Fatalf("expected excerpt for configureRouter")
	}

	tokens := tokenbudget.EstimateTokens(out)
	if tokens > excerptMaxTokens {
		t.Errorf("truncated excerpt still over cap: %d tokens > %d (truncation marker missing?)",
			tokens, excerptMaxTokens)
	}

	if !regexp.MustCompile(`// \.\.\. \d+ lines elided in extracted excerpt \.\.\.`).MatchString(out) {
		t.Errorf("expected elision marker in oversized excerpt output:\n%s", out)
	}
	// Function signature (head) and closing brace + return (tail) must survive.
	if !strings.Contains(out, "func configureRouter() error") {
		t.Errorf("head dropped function signature:\n%s", out)
	}
	if !strings.Contains(out, "return nil") {
		t.Errorf("tail dropped return statement:\n%s", out)
	}
}

// TestMergeOverlappingStrictDoesNotMergeNearby asserts the helper
// directly: two blocks separated by a one-line gap stay separate.
// mergeOverlapping (gap=2) would have merged them.
func TestMergeOverlappingStrictDoesNotMergeNearby(t *testing.T) {
	in := []block{
		{startLine: 10, endLine: 20, symbol: "A"},
		{startLine: 22, endLine: 30, symbol: "B"},
	}
	got := mergeOverlappingStrict(in)
	if len(got) != 2 {
		t.Fatalf("strict merge fused near blocks (gap=1); got %d blocks", len(got))
	}
	gotLoose := mergeOverlapping(in)
	if len(gotLoose) != 1 {
		t.Fatalf("loose merge should still fuse gap≤2; got %d blocks", len(gotLoose))
	}
}
