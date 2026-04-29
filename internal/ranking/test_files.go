package ranking

import (
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// testDownrankFactor scales the score of test-like files when the query has
// no apparent test intent. 0.72 is soft enough that a strongly matching test
// file still surfaces, but reliably ranks below a production file with the
// same matches. Picked over a fixed subtraction so the penalty stays
// proportional to the file's own score.
const testDownrankFactor = 0.72

// testLikeDirs are directory names that, when present anywhere in a path,
// mark the file as test-like. Some are already filtered by the walker
// (testdata, fixtures); they remain here so the helper's semantics match the
// spec regardless of walker configuration.
var testLikeDirs = map[string]bool{
	"__tests__": true,
	"tests":     true,
	"test":      true,
	"fixtures":  true,
	"mocks":     true,
	"testdata":  true,
}

// testLikeSuffixes identifies test files by basename suffix. Listed in full
// rather than reconstructed via splits so the rule reads as data and is
// trivial to extend.
var testLikeSuffixes = []string{
	"_test.go",
	".test.ts", ".test.tsx", ".test.js", ".test.jsx",
	".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
}

// testIntentTerms are query tokens that signal the user is asking about
// tests, fixtures, or coverage. Stems are included explicitly so we don't
// rely on the ranking stemmer reducing them to a common form.
var testIntentTerms = map[string]bool{
	"test": true, "tests": true, "testing": true, "tested": true,
	"fixture": true, "fixtures": true,
	"mock": true, "mocks": true, "mocking": true, "mocked": true,
	"benchmark": true, "benchmarks": true,
	"coverage":   true,
	"regression": true, "regressions": true,
	"golden": true,
}

// isTestLikePath reports whether relPath looks like a test, fixture, or mock
// file. Paths are normalised to forward slashes and lowercased so behaviour
// is identical on Windows and Unix.
func isTestLikePath(relPath string) bool {
	rel := strings.ToLower(filepath.ToSlash(relPath))
	if rel == "" {
		return false
	}
	base := filepath.Base(rel)
	for _, suf := range testLikeSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	for _, part := range strings.Split(rel, "/") {
		if testLikeDirs[part] {
			return true
		}
	}
	return false
}

// queryWantsTests reports whether the query contains a token signalling test
// intent. The match is on whole tokens (split on non-alphanumeric chars) so
// "latest" or "request" do not falsely trigger via substring match against
// "test".
func queryWantsTests(query string) bool {
	if query == "" {
		return false
	}
	lower := strings.ToLower(query)
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') && r != '_'
	})
	for _, w := range words {
		if testIntentTerms[w] {
			return true
		}
	}
	return false
}

// applyTestPenalty multiplies the score of test-like files by
// testDownrankFactor when the query has no test intent, and tags every
// test-like file with an audit reason in either case so an `ask --explain`
// run shows why a test was or was not penalised.
//
// Files with non-positive scores are skipped: scaling 0 changes nothing, and
// negative values aren't part of the current scoring contract.
func applyTestPenalty(scored []models.ScoredFile, query string) {
	wantsTests := queryWantsTests(query)
	for i := range scored {
		if !isTestLikePath(scored[i].Record.RelPath) {
			continue
		}
		if wantsTests {
			scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
				Signal: "query_test_intent_detected",
				Detail: scored[i].Record.RelPath,
				Weight: 0,
			})
			continue
		}
		if scored[i].Score <= 0 {
			continue
		}
		before := scored[i].Score
		scored[i].Score = before * testDownrankFactor
		scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
			Signal: "test_like_downrank",
			Detail: scored[i].Record.RelPath,
			Weight: scored[i].Score - before, // negative delta
		})
	}
}
