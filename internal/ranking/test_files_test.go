package ranking

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestIsTestLikePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// suffixes
		{"internal/parser/parser_test.go", true},
		{"src/auth.test.ts", true},
		{"src/auth.test.tsx", true},
		{"src/auth.test.js", true},
		{"src/auth.test.jsx", true},
		{"src/auth.spec.ts", true},
		{"src/auth.spec.tsx", true},
		{"src/auth.spec.js", true},
		{"src/auth.spec.jsx", true},
		// directories
		{"src/__tests__/auth.ts", true},
		{"tests/integration/auth.go", true},
		{"pkg/test/helpers.go", true},
		{"fixtures/sample.json", true},
		{"src/mocks/router.ts", true},
		{"testdata/golden.txt", true},
		// negatives — production paths whose names brush up against the rules
		{"internal/parser/parser.go", false},
		{"src/auth.ts", false},
		{"src/contest.ts", false},        // contains "test" as substring, not a dir part
		{"src/latest_release.ts", false}, // "latest" must not trigger
		{"src/spectrum.ts", false},       // "spec" prefix must not trigger
		{"src/testify.go", false},        // "test" inside basename, not a suffix
		{"", false},
	}
	for _, c := range cases {
		if got := isTestLikePath(c.path); got != c.want {
			t.Errorf("isTestLikePath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestQueryWantsTests(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"how is authentication tested?", true},
		{"failing test for parser", true},
		{"unit tests for ranker", true},
		{"add a fixture for the bundle", true},
		{"mock the database", true},
		{"benchmark the packager", true},
		{"coverage report", true},
		{"regression on Go const blocks", true},
		{"golden output mismatch", true},
		// negatives — implementation queries that mention adjacent words
		{"how is authentication configured?", false},
		{"latest release notes", false},
		{"contest the score", false},
		{"request handling", false},
		{"specification of the bundle format", false},
		{"", false},
	}
	for _, c := range cases {
		if got := queryWantsTests(c.query); got != c.want {
			t.Errorf("queryWantsTests(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

// TestRankDownranksTestsOnImplementationQuery is the headline scenario:
// "how is authentication configured?" must surface auth.go above auth_test.go
// even though both files contain matching symbols.
func TestRankDownranksTestsOnImplementationQuery(t *testing.T) {
	files := []models.FileRecord{
		{
			RelPath: "internal/auth/auth.go",
			Lang:    models.LangGo,
			Symbols: []models.Symbol{
				{Name: "Authenticate", Kind: "func"},
				{Name: "configureAuth", Kind: "func"},
			},
		},
		{
			RelPath: "internal/auth/auth_test.go",
			Lang:    models.LangGo,
			Symbols: []models.Symbol{
				{Name: "TestAuthenticate", Kind: "func"},
				{Name: "TestConfigureAuth", Kind: "func"},
				{Name: "setupAuth", Kind: "func"},
			},
		},
	}

	ranked := Rank(files, "how is authentication configured?")

	if ranked[0].Record.RelPath != "internal/auth/auth.go" {
		t.Fatalf("expected auth.go first, got %s (scores: %.2f vs %.2f)",
			ranked[0].Record.RelPath, ranked[0].Score, ranked[1].Score)
	}

	// Test file must carry the audit reason explaining the demotion.
	var found bool
	for _, r := range ranked[1].Reasons {
		if r.Signal == "test_like_downrank" {
			found = true
			if r.Weight >= 0 {
				t.Errorf("test_like_downrank weight should be negative delta, got %.2f", r.Weight)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected test_like_downrank reason on auth_test.go, reasons: %v", ranked[1].Reasons)
	}
}

// TestRankPreservesTestsOnTestQuery confirms that an explicit test-intent
// query disables the penalty: auth_test.go is now allowed to outrank auth.go
// (it has more matching symbols), and gets a query_test_intent_detected
// reason instead of a downrank.
func TestRankPreservesTestsOnTestQuery(t *testing.T) {
	files := []models.FileRecord{
		{
			RelPath: "internal/auth/auth.go",
			Lang:    models.LangGo,
			Symbols: []models.Symbol{
				{Name: "Authenticate", Kind: "func"},
			},
		},
		{
			RelPath: "internal/auth/auth_test.go",
			Lang:    models.LangGo,
			Symbols: []models.Symbol{
				{Name: "TestAuthenticate", Kind: "func"},
				{Name: "TestAuthenticateFailure", Kind: "func"},
				{Name: "setupAuth", Kind: "func"},
			},
		},
	}

	ranked := Rank(files, "how is authentication tested?")

	// auth_test.go must not carry test_like_downrank.
	var testFile models.ScoredFile
	for _, r := range ranked {
		if r.Record.RelPath == "internal/auth/auth_test.go" {
			testFile = r
			break
		}
	}
	for _, r := range testFile.Reasons {
		if r.Signal == "test_like_downrank" {
			t.Errorf("auth_test.go should not be downranked when query asks about tests, got reason %v", r)
		}
	}

	// And it must carry the explicit intent-detected marker.
	var marked bool
	for _, r := range testFile.Reasons {
		if r.Signal == "query_test_intent_detected" {
			marked = true
			break
		}
	}
	if !marked {
		t.Errorf("expected query_test_intent_detected reason on auth_test.go, reasons: %v", testFile.Reasons)
	}
}

// TestApplyTestPenaltyIdempotentOnZeroScores guards against the penalty
// silently mutating scores it has nothing useful to do with.
func TestApplyTestPenaltyIdempotentOnZeroScores(t *testing.T) {
	scored := []models.ScoredFile{
		{Record: models.FileRecord{RelPath: "src/auth.test.ts"}, Score: 0},
	}
	applyTestPenalty(scored, "implementation query")
	if scored[0].Score != 0 {
		t.Errorf("zero score should remain zero, got %.2f", scored[0].Score)
	}
}
