package packager

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

// rec is a tiny helper for building a FileRecord with hand-picked symbol
// line numbers. The packager's real path runs the parser to produce these,
// but excerpt.go consumes them directly so tests bypass that step and
// pin the input precisely.
func rec(rel string, lang models.Lang, syms ...models.Symbol) models.FileRecord {
	return models.FileRecord{
		RelPath: rel,
		Lang:    lang,
		Symbols: syms,
	}
}

func TestExtractExcerpt_TS_PicksMatchingFunctionBody(t *testing.T) {
	content := strings.Join([]string{
		"// 1",
		"export function unrelatedHelper(): void {", // line 2
		"  console.log('noise');",
		"}",
		"",                                              // 5
		"export function runSearch(q: string): string { // line 6",
		"  if (!q) return 'empty';",
		"  const result = doStuff(q);",
		"  return result;",
		"}",                                  // 10
		"",                                   // 11
		"function doStuff(q: string) {",      // 12
		"  return q.toUpperCase();",
		"}",
	}, "\n")
	r := rec("src/app.ts", models.LangTypeScript,
		models.Symbol{Name: "unrelatedHelper", Kind: "export_func", Line: 2},
		models.Symbol{Name: "runSearch", Kind: "export_func", Line: 6},
		models.Symbol{Name: "doStuff", Kind: "func", Line: 12},
	)

	out, ok := extractExcerpt(r, content, []string{"search"})
	if !ok {
		t.Fatalf("expected an excerpt, got none")
	}
	if !strings.Contains(out, "runSearch") {
		t.Errorf("excerpt should contain the matching symbol body; got:\n%s", out)
	}
	if strings.Contains(out, "unrelatedHelper") {
		t.Errorf("excerpt should NOT include unrelated symbols; got:\n%s", out)
	}
	if strings.Contains(out, "doStuff(") && !strings.Contains(out, "// ── ") {
		t.Errorf("missing block marker; output not properly framed:\n%s", out)
	}
	if !strings.Contains(out, "lines omitted") {
		t.Errorf("excerpt should mark elided lines so the model knows it's a partial view; got:\n%s", out)
	}
}

func TestExtractExcerpt_TS_MergesAdjacentMatches(t *testing.T) {
	// Two matching functions on consecutive line ranges should coalesce
	// into a single block so the prompt does not waste tokens on a
	// "1 lines omitted" marker between them.
	content := strings.Join([]string{
		"function alpha() {", // 1
		"  return 1;",
		"}",
		"function beta() {", // 4
		"  return 2;",
		"}",
	}, "\n")
	r := rec("src/x.ts", models.LangJavaScript,
		models.Symbol{Name: "alpha", Kind: "func", Line: 1},
		models.Symbol{Name: "beta", Kind: "func", Line: 4},
	)
	out, ok := extractExcerpt(r, content, []string{"alpha", "beta"})
	if !ok {
		t.Fatalf("expected excerpt")
	}
	// Exactly one ── marker means the blocks merged.
	if got := strings.Count(out, "// ── "); got != 1 {
		t.Errorf("expected merged single block, got %d markers:\n%s", got, out)
	}
	if !strings.Contains(out, "alpha, beta") && !strings.Contains(out, "beta, alpha") {
		t.Errorf("merged block should list both symbol names; got:\n%s", out)
	}
}

func TestExtractExcerpt_TS_QualifiedClassMethod(t *testing.T) {
	// The parser emits class methods as "ClassName.methodName". A query
	// for the method name alone should still match.
	content := strings.Join([]string{
		"class AuthService {", // 1
		"  authenticate(user: string) {",
		"    return 'token';",
		"  }",
		"  logout() { /* noop */ }", // 5
		"}",
	}, "\n")
	r := rec("src/auth.ts", models.LangTypeScript,
		models.Symbol{Name: "AuthService", Kind: "class", Line: 1},
		models.Symbol{Name: "AuthService.authenticate", Kind: "method", Line: 2},
		models.Symbol{Name: "AuthService.logout", Kind: "method", Line: 5},
	)
	out, ok := extractExcerpt(r, content, []string{"authenticate"})
	if !ok {
		t.Fatalf("expected excerpt for qualified method match")
	}
	if !strings.Contains(out, "authenticate(user") {
		t.Errorf("excerpt should include the matched method body; got:\n%s", out)
	}
}

func TestExtractExcerpt_Python_IndentBlock(t *testing.T) {
	content := strings.Join([]string{
		"def helper():",
		"    return 0",
		"",
		"def compute_score(x):", // line 4
		"    if x < 0:",
		"        return 0",
		"    return x * 2",
		"",
		"def other():", // 9
		"    pass",
	}, "\n")
	r := rec("src/score.py", models.LangPython,
		models.Symbol{Name: "helper", Kind: "func", Line: 1},
		models.Symbol{Name: "compute_score", Kind: "func", Line: 4},
		models.Symbol{Name: "other", Kind: "func", Line: 9},
	)
	out, ok := extractExcerpt(r, content, []string{"score"})
	if !ok {
		t.Fatalf("expected excerpt")
	}
	if !strings.Contains(out, "def compute_score(x):") {
		t.Errorf("missing matched def header:\n%s", out)
	}
	if !strings.Contains(out, "return x * 2") {
		t.Errorf("indent block did not capture full body:\n%s", out)
	}
	if strings.Contains(out, "def other():") {
		t.Errorf("indent block leaked into the next def:\n%s", out)
	}
}

func TestExtractExcerpt_NoMatchReturnsFalse(t *testing.T) {
	content := "function foo() { return 1; }\n"
	r := rec("src/foo.ts", models.LangTypeScript,
		models.Symbol{Name: "foo", Kind: "func", Line: 1},
	)
	if _, ok := extractExcerpt(r, content, []string{"bar", "baz"}); ok {
		t.Errorf("non-matching terms must return ok=false (caller falls back to signature)")
	}
}

func TestExtractExcerpt_UnsupportedLangReturnsFalse(t *testing.T) {
	// JSON / YAML / Markdown / unknown have no scope walker — the
	// extractor must reject cleanly so the caller falls back to
	// signature, not to broken code.
	content := "{ \"foo\": 1 }\n"
	r := rec("src/data.json", models.LangJSON,
		models.Symbol{Name: "foo", Kind: "key", Line: 1},
	)
	if _, ok := extractExcerpt(r, content, []string{"foo"}); ok {
		t.Errorf("JSON is not supported for excerpts; expected ok=false")
	}
}

// TestMatchingSymbols_VariantsBeatLiteralSubstring is the regression
// suite for the variant-aware symbol matcher. Each row carries:
//
//   - want: the new symmetric-containment behaviour (with stems +
//     reverse direction) — what the matcher MUST return.
//   - legacyLiteral: what the previous strings.Contains(symbolName, term)
//     check would have returned. The test self-checks this column too,
//     so the diff between the two columns is exactly what this iteration
//     improves: every row where want=true and legacyLiteral=false is
//     a query that now finds a top-3 file's relevant body where it
//     previously fell back to a bare signature.
func TestMatchingSymbols_VariantsBeatLiteralSubstring(t *testing.T) {
	type row struct {
		term, symbol  string
		want          bool // new behaviour (this iteration)
		legacyLiteral bool // strings.Contains(symbol_lower, term_lower)
		why           string
	}
	rows := []row{
		// The four cases the user explicitly listed.
		{"authentication", "auth", true, false, "reverse: query contains symbol"},
		{"configuration", "config", true, false, "reverse: query contains symbol"},
		{"rendering", "render", true, false, "forward: stem(rendering)=render matches symbol"},
		{"journal", "journal", true, true, "trivial equality (regression guard)"},

		// Bonus coverage that falls out of the same machinery.
		{"rendering", "renderer", true, false, "forward: stem(rendering)=render ⊂ renderer"},
		{"queries", "buildQuery", true, false, "forward: stem(queries)=query ⊂ buildquery (lowercased)"},
		{"journal", "loadJournal", true, true, "literal: 'journal' is a substring (regression guard)"},
		// authMiddleware does NOT match "authentication" today because we
		// do not split camelCase yet. Documented as a deliberate non-goal
		// for this iteration; pinned here so the day we add camel splitting
		// this row flips to want=true and we notice. Bare "auth" symbols
		// (above) already match via reverse containment.
		{"authentication", "authMiddleware", false, false, "would need camelCase split (next iteration)"},

		// Negative cases: must STAY non-matches so we did not relax too much.
		{"authentication", "userManager", false, false, "no shared root — must stay false"},
		{"render", "scoreCard", false, false, "no shared root — must stay false"},
		// "go" is too short for the 3-char floor on either direction. The
		// new matcher rejects it. The literal substring check, lacking the
		// floor, would have produced a false positive — the floor is the
		// concrete win this column captures.
		{"go", "goSomething", false, true, "term too short (<3) — floor blocks the false positive that literal would have"},
	}
	for _, r := range rows {
		t.Run(r.term+"_"+r.symbol, func(t *testing.T) {
			sym := models.Symbol{Name: r.symbol, Kind: "func", Line: 1}
			got := len(matchingSymbols([]models.Symbol{sym}, []string{r.term})) > 0
			if got != r.want {
				t.Errorf("matchingSymbols(%q, %q) = %v; want %v (%s)",
					r.symbol, r.term, got, r.want, r.why)
			}
			// Self-check: the legacy column is honest about what the
			// previous literal-substring matcher would have done.
			legacy := strings.Contains(strings.ToLower(r.symbol), r.term)
			if legacy != r.legacyLiteral {
				t.Errorf("legacy column wrong for (%q, %q): test says %v, actual literal substring=%v",
					r.symbol, r.term, r.legacyLiteral, legacy)
			}
		})
	}
}

func TestMatchingSymbols_QualifiedNamesUseEachComponent(t *testing.T) {
	// The parser emits class methods as "ClassName.methodName". Variant
	// matching must apply to either component independently — querying
	// "rendering" should find AuthService.renderRow without querying the
	// class name. This complements the existing
	// TestExtractExcerpt_TS_QualifiedClassMethod which asserts on the
	// extractor; this one isolates the matcher.
	syms := []models.Symbol{
		{Name: "AuthService.renderRow", Kind: "method", Line: 5},
		{Name: "AuthService.unrelated", Kind: "method", Line: 9},
	}
	got := matchingSymbols(syms, []string{"rendering"})
	if len(got) != 1 || got[0].Name != "AuthService.renderRow" {
		t.Errorf("expected AuthService.renderRow only; got %+v", got)
	}
}

func TestBraceBlock_StringsAndCommentsDoNotTripBraceCounter(t *testing.T) {
	// Mock the kind of lines that broke a naive counter: braces hidden
	// inside strings and comments must not count toward depth.
	content := strings.Join([]string{
		"function tricky() {",                                   // 1
		"  const a = '} not a real close';",                     //
		"  /* } also not real */",                               //
		"  const b = \"another } string\";",                     //
		"  // } in line comment",                                //
		"  if (true) { console.log('nested'); }",                //
		"  return 1;",                                           //
		"}",                                                     // 8
		"function next() { return 2; }",                         // 9
	}, "\n")
	b := braceBlock(models.Symbol{Name: "tricky", Line: 1}, strings.Split(content, "\n"))
	if b.startLine != 1 || b.endLine != 8 {
		t.Errorf("braceBlock should have captured lines 1-8 of tricky(); got %d-%d", b.startLine, b.endLine)
	}
}
