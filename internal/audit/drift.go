package audit

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// identifierPattern captures code-shaped spans: camelCase, snake_case, or
// dotted names (jwt.sign, os.path). Used on both the response and the
// bundle so both sides see the same tokenisation.
var identifierPattern = regexp.MustCompile(
	`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`,
)

// tokenKind is the drift category a response token belongs to. The
// classifier groups tokens into four buckets so the report can show which
// axis drift is happening on — path hallucinations behave very differently
// than API-name hallucinations, and users often care about the distinction.
type tokenKind int

const (
	kindSkip   tokenKind = iota // narrative / non-code token; never checked
	kindPath                    // has slash or known extension
	kindAPI                     // dotted name like jwt.sign, os.readFile
	kindSymbol                  // camelCase / snake_case / PascalCase / ALL_CAPS
)

// DetectDrift flags identifiers in the response that are not present in the
// bundle. The check is deliberately conservative: a single Capitalised
// word like "This", "Here" or "Overall" is never treated as a code claim,
// because English narrative sprinkles those in naturally. Real code tokens
// carry strong shape signals (underscores, 2+ capitals, camelCase) and are
// the only ones that reach the membership check.
//
// The function never errors — empty or all-prose input simply yields a
// zero DriftReport. That is a legitimate answer, not a failure.
func DetectDrift(response string, bundle models.Bundle) DriftReport {
	if strings.TrimSpace(response) == "" {
		return DriftReport{}
	}

	known := collectKnownTokens(bundle)
	paths, apis, syms := extractCandidates(response)

	var report DriftReport
	unknownPaths := make(map[string]bool)
	unknownAPIs := make(map[string]bool)
	unknownSyms := make(map[string]bool)

	for p := range paths {
		if isKnownPath(p, bundle) {
			report.KnownCount++
		} else {
			unknownPaths[p] = true
		}
	}
	for a := range apis {
		if known[strings.ToLower(a)] {
			report.KnownCount++
		} else {
			unknownAPIs[a] = true
		}
	}
	for s := range syms {
		if known[strings.ToLower(s)] {
			report.KnownCount++
		} else {
			unknownSyms[s] = true
		}
	}

	report.UnknownPaths = sortedKeys(unknownPaths)
	report.UnknownAPIs = sortedKeys(unknownAPIs)
	report.UnknownSymbols = sortedKeys(unknownSyms)
	report.UnknownCount = len(report.UnknownPaths) + len(report.UnknownAPIs) + len(report.UnknownSymbols)

	if total := report.KnownCount + report.UnknownCount; total > 0 {
		report.Rate = float64(report.UnknownCount) / float64(total)
	}
	return report
}

// collectKnownTokens returns the lower-cased set of every identifier that
// appears in the bundle: the full token plus the sub-words we get from
// splitting camelCase / snake_case / dotted names. Membership here is what
// disproves drift for the symbol and API buckets.
func collectKnownTokens(bundle models.Bundle) map[string]bool {
	known := make(map[string]bool, 256)
	for _, f := range bundle.Fragments {
		for _, tok := range identifierPattern.FindAllString(f.Content, -1) {
			known[strings.ToLower(tok)] = true
			for _, piece := range splitIdentifier(tok) {
				known[strings.ToLower(piece)] = true
			}
		}
	}
	return known
}

// extractCandidates tokenises the response and partitions every candidate
// into the three drift buckets. Skipped tokens (prose, single capitalised
// words) never reach the membership check — this is the main lever for
// false-positive reduction.
//
// Paths come from two sources: the citation regex (so we pick up things
// like `utils.py` that also have an extension) and any identifier token
// that contains a slash or known extension. Dotted non-file tokens become
// API candidates; the rest go through the symbol classifier.
func extractCandidates(response string) (paths, apis, syms map[string]bool) {
	paths = make(map[string]bool)
	apis = make(map[string]bool)
	syms = make(map[string]bool)

	for _, c := range ParseCitations(response) {
		paths[c.RelPath] = true
	}

	for _, tok := range identifierPattern.FindAllString(response, -1) {
		switch classifyToken(tok) {
		case kindPath:
			paths[strings.TrimPrefix(tok, "./")] = true
		case kindAPI:
			apis[tok] = true
		case kindSymbol:
			syms[tok] = true
		}
	}
	paths = dedupePathAliases(paths)
	return
}

// dedupePathAliases collapses `bar.ts` into `foo/bar.ts` when both appear.
// Both land in the paths map — one from the citation regex (which accepts
// slashes), the other from the identifier pattern (which does not) — and
// we'd otherwise count the same reference twice against drift rate.
func dedupePathAliases(paths map[string]bool) map[string]bool {
	if len(paths) < 2 {
		return paths
	}
	bases := make(map[string]bool, len(paths))
	for p := range paths {
		if strings.Contains(p, "/") {
			bases[filepath.Base(p)] = true
		}
	}
	out := make(map[string]bool, len(paths))
	for p := range paths {
		if !strings.Contains(p, "/") && bases[p] {
			continue
		}
		out[p] = true
	}
	return out
}

// classifyToken routes a token into its drift bucket. Ordering matters:
// path features (slash, known extension) win over everything so `foo.py`
// never lands as an API; dotted-with-segments wins over plain symbol so
// `jwt.sign` is an API rather than two separate tokens.
func classifyToken(tok string) tokenKind {
	if strings.Contains(tok, "/") || hasKnownExt(tok) {
		return kindPath
	}
	if looksDotted(tok) {
		return kindAPI
	}
	if looksCodeSymbol(tok) {
		return kindSymbol
	}
	return kindSkip
}

// looksDotted recognises API-style dotted names (`jwt.sign`, `os.path.join`,
// `config.Foo`). Every segment must be at least 2 chars and identifier-
// shaped; the 2-char minimum weeds out prose artefacts the tokeniser might
// have pulled in like `e.g` or `i.e`.
func looksDotted(tok string) bool {
	if !strings.Contains(tok, ".") {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if len(p) < 2 {
			return false
		}
	}
	return true
}

// looksCodeSymbol classifies plain (non-dotted, non-path) identifiers.
// The core rule: a single Capitalised-lowercase word is NEVER codey,
// because that shape dominates English narrative (`This`, `Here`, `When`,
// `Overall`, `Usually`, …). Real code tokens appear as:
//
//   - snake_case (has `_`)                          → accept
//   - PascalCase / CONST_CASE (2+ capital letters)  → accept
//   - camelCase (lowercase start, ≥1 later capital) → accept
//   - SCREAMING_CASE (all-upper, ≥4 chars)          → accept
//
// We lose single-word PascalCase references like `Bundle` or `Router` —
// that's a deliberate trade-off. In practice developers write those as
// `BundleStats`, `Bundle.Files`, or lowercase `bundle`, all of which still
// land in one of the accepted shapes. Keeping the rule categorical keeps
// the detector interpretable: there are no hidden stopword lists or
// frequency heuristics.
func looksCodeSymbol(tok string) bool {
	if len(tok) < 4 {
		return false
	}
	var upper, lower, under int
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		case r == '_':
			under++
		}
	}
	if under > 0 {
		return true
	}
	if upper >= 2 {
		return true
	}
	if upper > 0 && lower == 0 {
		return true // SCREAMING or plain all-caps acronym of length ≥ 4
	}
	if upper == 1 && lower > 0 {
		// Accept only camelCase (lowercase start); reject single-capitalised
		// words regardless of length — they're overwhelmingly prose.
		first := rune(tok[0])
		return first >= 'a' && first <= 'z'
	}
	return false
}

// isKnownPath checks a path candidate against the bundle by full relpath or
// basename. Kept separate from the lower-cased token set because paths are
// case-sensitive on non-mac filesystems.
func isKnownPath(path string, bundle models.Bundle) bool {
	norm := filepath.ToSlash(strings.TrimPrefix(path, "./"))
	base := filepath.Base(norm)
	for _, f := range bundle.Fragments {
		rel := filepath.ToSlash(f.RelPath)
		if rel == norm || filepath.Base(rel) == base {
			return true
		}
	}
	return false
}

// splitIdentifier breaks a token like `hashPasswordV2` or `hash_password`
// into its component words. Short pieces (< 3 chars) are dropped to avoid
// making every capital letter a known token.
func splitIdentifier(tok string) []string {
	var words []string
	var cur strings.Builder
	for i, r := range tok {
		isBoundary := r == '_' || r == '.'
		isUpper := r >= 'A' && r <= 'Z'
		if isBoundary {
			if cur.Len() >= 3 {
				words = append(words, cur.String())
			}
			cur.Reset()
			continue
		}
		if isUpper && i > 0 && cur.Len() > 0 {
			prev := cur.String()[cur.Len()-1]
			if prev >= 'a' && prev <= 'z' {
				if cur.Len() >= 3 {
					words = append(words, cur.String())
				}
				cur.Reset()
			}
		}
		cur.WriteRune(r)
	}
	if cur.Len() >= 3 {
		words = append(words, cur.String())
	}
	return words
}

// hasKnownExt reports whether a token ends in one of the extensions the
// citation pattern recognises. Used to treat bare `utils.py` as a path even
// when it appears without a leading slash.
func hasKnownExt(tok string) bool {
	lower := strings.ToLower(tok)
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".go", ".md", ".json", ".yaml", ".yml"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
