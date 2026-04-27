package output_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
)

func TestWriteClaudeStructure(t *testing.T) {
	b := models.Bundle{
		Query:  "why does ranking pick utils.py",
		Budget: 3000,
		Fragments: []models.ContextFragment{{
			RelPath:        "internal/ranking/ranking.go",
			Lang:           models.LangGo,
			Representation: models.RepSignature,
			Tokens:         42,
			Content:        "func Rank(files, query) []ScoredFile",
			Reasons: []models.InclusionReason{
				{Signal: "filename_match", Detail: "ranking", Weight: 3.0},
				{Signal: "focus", Detail: "internal/ranking", Weight: 4.0},
			},
		}},
		Stats: models.BundleStats{
			FilesConsidered:  8,
			FilesIncluded:    1,
			TokensUsed:       120,
			TokensBudget:     3000,
			CompressionRatio: 3.2,
		},
	}
	summary := output.RepoSummary{
		Name:      "neurofs",
		Files:     42,
		Symbols:   200,
		Entry:     "cmd/neurofs/main.go",
		Languages: map[string]int{"go": 30, "typescript": 8, "python": 4},
	}

	var buf bytes.Buffer
	if err := output.WriteClaude(&buf, b, summary); err != nil {
		t.Fatalf("WriteClaude: %v", err)
	}
	got := buf.String()

	// Hard requirements: the exact shape Claude needs to ground on.
	for _, want := range []string{
		"<task>", "</task>",
		"why does ranking pick utils.py",
		"<repo>", "name: neurofs", "languages:", "go:30",
		"<selection>", "bundle:",
		"<context>", `path="internal/ranking/ranking.go"`, `rep="signature"`,
		`reasons="focus,filename_match"`, // sorted by aggregate weight
		"<instructions>", "cite it as `path:line`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("claude output missing %q\n---\n%s", want, got)
		}
	}

	// Repo tag must close exactly once.
	if strings.Count(got, "</repo>") != 1 {
		t.Errorf("expected one </repo>, output:\n%s", got)
	}
}

func TestWriteClaudeSkipsEmptyRepoSummary(t *testing.T) {
	b := models.Bundle{Query: "x"}
	var buf bytes.Buffer
	if err := output.WriteClaude(&buf, b, output.RepoSummary{}); err != nil {
		t.Fatalf("WriteClaude: %v", err)
	}
	if strings.Contains(buf.String(), "<repo>") {
		t.Errorf("empty summary should not emit <repo> block:\n%s", buf.String())
	}
}

// TestWriteClaudeInstructionsCoverExcerpt locks the instruction wording for
// rep="excerpt". Model behaviour observed without these instructions: when
// the bundle contains an excerpt, the receiving model often treats the
// shown blocks as the whole file and over-confidently asserts that elided
// lines do not contain X. The instruction block must:
//
//  1. Name `excerpt` explicitly so the model recognises the rep value.
//  2. Frame excerpt as a partial view ("do not assume unseen code").
//  3. Tell the model how to ask for more (full file OR wider excerpt).
//
// Phrased as substring assertions so paraphrasing later is fine as long as
// the contract is preserved.
func TestWriteClaudeInstructionsCoverExcerpt(t *testing.T) {
	b := models.Bundle{Query: "anything"}
	var buf bytes.Buffer
	if err := output.WriteClaude(&buf, b, output.RepoSummary{}); err != nil {
		t.Fatalf("WriteClaude: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"<instructions>",
		"`excerpt`",                            // rep value mentioned literally
		"partial view",                         // framing
		"do not assume unseen code",            // anti-hallucination clause
		"`signature`, `structural_note`, or `excerpt`", // grouped guidance
		"ask for the full file or a wider excerpt",     // remediation path
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions missing %q\n---\n%s", want, got)
		}
	}

	// Existing contract that older tests already enforce, re-checked here
	// in isolation so a future edit cannot drop both at once.
	if !strings.Contains(got, "cite it as `path:line`") {
		t.Errorf("instructions dropped citation rule")
	}
}

// TestWriteClaudeExcerptFragmentSurfacesItsMarkers proves the writer is a
// transparent envelope for an excerpt's self-describing markers — the file
// path, representation tag, line range header, and omitted-lines markers
// produced by packager.renderExcerpt all reach the prompt verbatim. The
// model needs every one of these to know which lines it is looking at and
// which it is NOT.
func TestWriteClaudeExcerptFragmentSurfacesItsMarkers(t *testing.T) {
	excerpt := `// file: src/auth.ts
// lang: typescript
// representation: excerpt

// ── src/auth.ts:12-18 (authenticate) ──
function authenticate(token) {
  if (!token) return null;
  return verify(token);
}

// ... 40 lines omitted ...

// ── src/auth.ts:60-65 (logout) ──
function logout() { /* ... */ }
`
	b := models.Bundle{
		Query: "auth flow",
		Fragments: []models.ContextFragment{{
			RelPath:        "src/auth.ts",
			Lang:           models.LangTypeScript,
			Representation: models.RepExcerpt,
			Tokens:         60,
			Content:        excerpt,
		}},
		Stats: models.BundleStats{FilesIncluded: 1, FilesConsidered: 5, TokensUsed: 60, TokensBudget: 4000},
	}
	var buf bytes.Buffer
	if err := output.WriteClaude(&buf, b, output.RepoSummary{}); err != nil {
		t.Fatalf("WriteClaude: %v", err)
	}
	got := buf.String()

	// XML attribute on the file tag: the model should read the rep value
	// without parsing the body.
	if !strings.Contains(got, `rep="excerpt"`) {
		t.Errorf("expected rep=\"excerpt\" attribute in file tag\n---\n%s", got)
	}
	// Self-describing body: every marker the extractor emits must survive.
	for _, want := range []string{
		"// representation: excerpt",                  // body header
		"// ── src/auth.ts:12-18 (authenticate) ──",   // line-range header
		"// ── src/auth.ts:60-65 (logout) ──",         // second block header
		"// ... 40 lines omitted ...",                 // elision marker
	} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt marker missing in writer output: %q\n---\n%s", want, got)
		}
	}
}

// TestWriteClaudeSignatureAndStructuralNoteUnchanged is a no-regression
// guard: the older partial-representation kinds must still pass through
// the writer without alteration, and they must still be covered by the
// (now broader) "ask if you need more" instruction.
func TestWriteClaudeSignatureAndStructuralNoteUnchanged(t *testing.T) {
	for _, rep := range []models.Representation{models.RepSignature, models.RepStructuralNote} {
		t.Run(string(rep), func(t *testing.T) {
			b := models.Bundle{
				Query: "x",
				Fragments: []models.ContextFragment{{
					RelPath:        "a.ts",
					Lang:           models.LangTypeScript,
					Representation: rep,
					Tokens:         10,
					Content:        "// some content",
				}},
			}
			var buf bytes.Buffer
			if err := output.WriteClaude(&buf, b, output.RepoSummary{}); err != nil {
				t.Fatalf("WriteClaude: %v", err)
			}
			got := buf.String()
			wantAttr := `rep="` + string(rep) + `"`
			if !strings.Contains(got, wantAttr) {
				t.Errorf("expected %s in file tag\n---\n%s", wantAttr, got)
			}
			// The grouped instruction still names this rep explicitly.
			if !strings.Contains(got, "`"+string(rep)+"`") {
				t.Errorf("instructions no longer name %q — model loses guidance for this rep\n---\n%s",
					rep, got)
			}
		})
	}
}

func TestWriteClaudeThroughDispatcher(t *testing.T) {
	// output.Write with FormatClaude must not panic and must produce a
	// task-first body even without a summary.
	var buf bytes.Buffer
	err := output.Write(&buf, models.Bundle{Query: "q"}, output.FormatClaude)
	if err != nil {
		t.Fatalf("Write(FormatClaude): %v", err)
	}
	if !strings.HasPrefix(buf.String(), "<task>") {
		t.Errorf("claude dispatcher should lead with <task>, got: %s", buf.String())
	}
}
