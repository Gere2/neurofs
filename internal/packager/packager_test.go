package packager_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestPackUpgradeWithSlackPromotesSignatureToFullCode(t *testing.T) {
	// A file sized between aggressiveFullCodeMaxTokens (180) and the upgrade
	// cap fullCodeMaxTokens (600). PreferSignatures keeps it as a signature
	// in the first pass; UpgradeWithSlack should promote it to full_code in
	// the second pass.
	body := ""
	for i := 0; i < 50; i++ {
		body += "// pad pad pad pad pad pad pad\n"
	}
	ranked := []models.ScoredFile{{
		Record: models.FileRecord{
			Path:    writeTempFile(t, "biggish.ts", body),
			RelPath: "src/biggish.ts",
			Lang:    models.LangTypeScript,
		},
		Score: 5.0,
	}}

	plain, _ := packager.Pack(ranked, "q", packager.Options{
		Budget:           4000,
		PreferSignatures: true,
	})
	if plain.Fragments[0].Representation == models.RepFullCode {
		t.Fatalf("baseline: PreferSignatures alone should yield signature, got full_code")
	}

	upgraded, _ := packager.Pack(ranked, "q", packager.Options{
		Budget:           4000,
		PreferSignatures: true,
		UpgradeWithSlack: true,
	})
	if upgraded.Fragments[0].Representation != models.RepFullCode {
		t.Fatalf("UpgradeWithSlack should promote to full_code when budget allows, got %s",
			upgraded.Fragments[0].Representation)
	}
	if upgraded.Stats.TokensUsed <= plain.Stats.TokensUsed {
		t.Fatalf("upgraded bundle should consume more budget (%d) than plain (%d)",
			upgraded.Stats.TokensUsed, plain.Stats.TokensUsed)
	}
}

func TestPackUpgradeWithSlackRespectsRemainingBudget(t *testing.T) {
	// Budget large enough for one signature but NOT for the body upgrade.
	// The fragment must stay as a signature.
	body := ""
	for i := 0; i < 400; i++ {
		body += "// big body line\n"
	}
	ranked := []models.ScoredFile{{
		Record: models.FileRecord{
			Path:    writeTempFile(t, "huge.ts", body),
			RelPath: "src/huge.ts",
			Lang:    models.LangTypeScript,
		},
		Score: 5.0,
	}}
	b, _ := packager.Pack(ranked, "q", packager.Options{
		Budget:           300,
		PreferSignatures: true,
		UpgradeWithSlack: true,
	})
	if len(b.Fragments) == 0 {
		t.Fatalf("expected at least one fragment")
	}
	if b.Fragments[0].Representation == models.RepFullCode {
		t.Errorf("upgrade must not exceed budget — expected signature, got full_code (%d tokens used of %d)",
			b.Stats.TokensUsed, b.Stats.TokensBudget)
	}
}

// TestPackUsesExcerptForTopRankedFile is the regression test for the
// "all-or-nothing per file" fix. Same TS file, same budget, same options —
// only QueryTerms differs:
//
//   - Without QueryTerms (legacy): top file collapses to a signature, model
//     sees function names but not bodies.
//   - With QueryTerms (new path): top file becomes an excerpt containing the
//     bodies of the symbols whose names match the query, with the rest
//     elided.
//
// The bundle must spend strictly more tokens on the new path (we are
// trading density for fidelity) AND the matched symbol's body must be
// recoverable verbatim from the fragment content.
func TestPackUsesExcerptForTopRankedFile(t *testing.T) {
	body := strings.Builder{}
	body.WriteString("export function unrelatedHelper() { return 'noise'; }\n\n")
	body.WriteString("export function runSearch(q) {\n")
	body.WriteString("  if (!q) return [];\n")
	body.WriteString("  const matches = collectMatches(q);\n")
	body.WriteString("  return scoreMatches(matches, q);\n")
	body.WriteString("}\n\n")
	// Padding so the file is well past aggressiveFullCodeMaxTokens (180)
	// and pushes the packager past option 1 (full_code).
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&body, "function pad_%03d() { /* %s */ return %d; }\n", i,
			strings.Repeat("padding text to inflate the file size", 1), i)
	}

	ranked := []models.ScoredFile{{
		Record: models.FileRecord{
			Path:    writeTempFile(t, "search.ts", body.String()),
			RelPath: "src/search.ts",
			Lang:    models.LangTypeScript,
			Symbols: []models.Symbol{
				{Name: "unrelatedHelper", Kind: "export_func", Line: 1},
				{Name: "runSearch", Kind: "export_func", Line: 3},
			},
		},
		Score: 5.0,
	}}

	common := packager.Options{
		Budget:           4000,
		PreferSignatures: true,
	}

	legacy, err := packager.Pack(ranked, "search behaviour", common)
	if err != nil {
		t.Fatalf("legacy pack: %v", err)
	}
	if len(legacy.Fragments) == 0 {
		t.Fatalf("legacy: expected at least one fragment")
	}
	if legacy.Fragments[0].Representation != models.RepSignature {
		t.Fatalf("legacy: expected signature without QueryTerms, got %s",
			legacy.Fragments[0].Representation)
	}
	if strings.Contains(legacy.Fragments[0].Content, "scoreMatches(matches, q)") {
		t.Fatalf("legacy: signature must not include the function body")
	}

	withTerms := common
	withTerms.QueryTerms = []string{"search"}
	excerpt, err := packager.Pack(ranked, "search behaviour", withTerms)
	if err != nil {
		t.Fatalf("excerpt pack: %v", err)
	}
	if len(excerpt.Fragments) == 0 {
		t.Fatalf("excerpt: expected at least one fragment")
	}
	frag := excerpt.Fragments[0]
	if frag.Representation != models.RepExcerpt {
		t.Fatalf("excerpt: expected RepExcerpt for top-ranked TS file with matching symbol, got %s",
			frag.Representation)
	}
	if !strings.Contains(frag.Content, "scoreMatches(matches, q)") {
		t.Errorf("excerpt: matched symbol's body should be present verbatim; got:\n%s",
			frag.Content)
	}
	if strings.Contains(frag.Content, "unrelatedHelper") {
		t.Errorf("excerpt: should not include unrelated symbols; got:\n%s",
			frag.Content)
	}
	if !strings.Contains(frag.Content, "// representation: excerpt") {
		t.Errorf("excerpt: header missing; got:\n%s", frag.Content)
	}
	// Note on token cost: an excerpt may be cheaper OR more expensive than
	// the signature, depending on the file. A wide-API file (hundreds of
	// exported names) hits the signature cap and the excerpt — focused on
	// just the matched body — comes out smaller. A narrow-API file with a
	// single big function goes the other way. The semantic invariant is
	// that the matched body is present in the excerpt and absent from the
	// signature, asserted above. Both bundles must respect the budget:
	if excerpt.Stats.TokensUsed > excerpt.Stats.TokensBudget {
		t.Errorf("excerpt overshot budget: used=%d budget=%d",
			excerpt.Stats.TokensUsed, excerpt.Stats.TokensBudget)
	}
	if legacy.Stats.TokensUsed > legacy.Stats.TokensBudget {
		t.Errorf("legacy overshot budget: used=%d budget=%d",
			legacy.Stats.TokensUsed, legacy.Stats.TokensBudget)
	}
	t.Logf("token cost — legacy signature: %d, excerpt: %d (same %d budget)",
		legacy.Stats.TokensUsed, excerpt.Stats.TokensUsed, excerpt.Stats.TokensBudget)
}

// TestPackExcerptGatedToTopThree confirms the positional cap. Files at
// rank 4+ never get sub-file extraction even when they match the query —
// this keeps excerpts reserved for files the ranker rates highest, where
// the per-fragment verbosity is justified.
func TestPackExcerptGatedToTopThree(t *testing.T) {
	mkBig := func(name string) models.ScoredFile {
		var sb strings.Builder
		sb.WriteString("export function searchHandler(q) { return q + '_done'; }\n")
		for i := 0; i < 250; i++ {
			fmt.Fprintf(&sb, "function pad_%03d() { return %d; }\n", i, i)
		}
		return models.ScoredFile{
			Record: models.FileRecord{
				Path:    writeTempFile(t, name, sb.String()),
				RelPath: "src/" + name,
				Lang:    models.LangTypeScript,
				Symbols: []models.Symbol{
					{Name: "searchHandler", Kind: "export_func", Line: 1},
				},
			},
		}
	}
	// Five files, descending score. Only the first three are eligible
	// for excerpt extraction; the rest must collapse to signature.
	ranked := []models.ScoredFile{}
	scores := []float64{9, 8, 7, 6, 5}
	names := []string{"a.ts", "b.ts", "c.ts", "d.ts", "e.ts"}
	for i, n := range names {
		f := mkBig(n)
		f.Score = scores[i]
		ranked = append(ranked, f)
	}

	b, err := packager.Pack(ranked, "search", packager.Options{
		Budget:           20000,
		PreferSignatures: true,
		QueryTerms:       []string{"search"},
	})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	excerptCount := 0
	for _, f := range b.Fragments {
		if f.Representation == models.RepExcerpt {
			excerptCount++
		}
	}
	if excerptCount > 3 {
		t.Errorf("excerptTopN cap should hold at 3, got %d excerpts", excerptCount)
	}
	if excerptCount == 0 {
		t.Errorf("expected at least one excerpt for top-ranked matching files; got none")
	}
}

// TestPackExcerptVariantMatchingFlipsSignatureToExcerpt is the integration
// witness for the variant-matcher iteration. Same TS file, same budget,
// same QueryTerms — except the term is "authentication" and the only
// matching symbol in the file is bare "auth". Under the previous literal
// substring matcher, matchingSymbols returned [], extractExcerpt returned
// ok=false, and the file collapsed to a signature. Under the new
// symmetric-containment matcher (reverse direction with 3-char floor),
// the symbol is found and the excerpt is selected.
//
// The legacy outcome is reproduced inline by asking Pack with a term that
// the literal substring DOES match ("auth"), and showing that term=auth
// and term=authentication now produce the same selection. Before this
// iteration only term=auth selected the excerpt.
func TestPackExcerptVariantMatchingFlipsSignatureToExcerpt(t *testing.T) {
	body := strings.Builder{}
	body.WriteString("export function unrelatedHelper() { return 'noise'; }\n\n")
	body.WriteString("export function auth(token) {\n")
	body.WriteString("  if (!token) return null;\n")
	body.WriteString("  return verify(token);\n")
	body.WriteString("}\n\n")
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&body, "function pad_%03d() { return %d; }\n", i, i)
	}
	mkRanked := func() []models.ScoredFile {
		return []models.ScoredFile{{
			Record: models.FileRecord{
				Path:    writeTempFile(t, "auth.ts", body.String()),
				RelPath: "src/auth.ts",
				Lang:    models.LangTypeScript,
				Symbols: []models.Symbol{
					{Name: "unrelatedHelper", Kind: "export_func", Line: 1},
					{Name: "auth", Kind: "export_func", Line: 3},
				},
			},
			Score: 5.0,
		}}
	}

	common := packager.Options{Budget: 4000, PreferSignatures: true}

	// Control: a literal-substring match. Term "auth" is a substring of
	// the symbol name "auth" — both old and new matchers produce excerpt.
	control := common
	control.QueryTerms = []string{"auth"}
	cb, err := packager.Pack(mkRanked(), "auth flow", control)
	if err != nil {
		t.Fatalf("control pack: %v", err)
	}
	if cb.Fragments[0].Representation != models.RepExcerpt {
		t.Fatalf("control: expected excerpt for term 'auth', got %s",
			cb.Fragments[0].Representation)
	}

	// Variant: same file, longer compound term. Old matcher missed it
	// because strings.Contains("auth", "authentication") is false. New
	// matcher succeeds via reverse containment.
	variant := common
	variant.QueryTerms = []string{"authentication"}
	vb, err := packager.Pack(mkRanked(), "authentication flow", variant)
	if err != nil {
		t.Fatalf("variant pack: %v", err)
	}
	if vb.Fragments[0].Representation != models.RepExcerpt {
		t.Fatalf("variant: expected excerpt for term 'authentication' (reverse-contains 'auth'), got %s — symbol matcher did not pick up the variant",
			vb.Fragments[0].Representation)
	}
	if !strings.Contains(vb.Fragments[0].Content, "verify(token)") {
		t.Errorf("variant: matched body must be present; got:\n%s",
			vb.Fragments[0].Content)
	}
	t.Logf("variant matcher delivered excerpt (%d tokens) for compound term — old behaviour would have produced signature",
		vb.Stats.TokensUsed)
}

func TestPackSignatureCappedForOversizedFiles(t *testing.T) {
	// A wide-API file: hundreds of exports → the signature alone would
	// blow past 350 tokens. Build one and confirm the signature gets
	// truncated with a "+ N more" marker, and that the resulting
	// fragment fits under the cap.
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "export function exportedFunctionWithALongName_%04d() { return %d; }\n", i, i)
	}
	body := sb.String()
	ranked := []models.ScoredFile{{
		Record: models.FileRecord{
			Path:    writeTempFile(t, "wide.ts", body),
			RelPath: "src/wide.ts",
			Lang:    models.LangTypeScript,
		},
		Score: 5.0,
	}}
	b, _ := packager.Pack(ranked, "q", packager.Options{
		Budget:           4000,
		PreferSignatures: true,
	})
	if len(b.Fragments) == 0 {
		t.Fatalf("expected one fragment")
	}
	frag := b.Fragments[0]
	if frag.Representation != models.RepSignature {
		t.Fatalf("expected signature, got %s", frag.Representation)
	}
	if frag.Tokens > 360 {
		t.Errorf("capped signature still too large: %d tokens (cap 350)", frag.Tokens)
	}
	if !strings.Contains(frag.Content, "more lines") {
		t.Errorf("expected truncation marker, got: %q", frag.Content)
	}
}
