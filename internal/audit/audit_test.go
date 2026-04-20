package audit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/models"
)

// fixtureBundle returns a small bundle good enough to exercise every audit
// code path without pulling in the packager or filesystem.
func fixtureBundle() models.Bundle {
	return models.Bundle{
		Query:  "how does authentication work",
		Budget: 4000,
		Fragments: []models.ContextFragment{
			{
				RelPath:        "src/auth.ts",
				Lang:           models.LangTypeScript,
				Representation: models.RepFullCode,
				Tokens:         120,
				Content:        "export function verifyToken(t: string) { return jwt.verify(t, SECRET); }\nexport const AuthMiddleware = () => {}",
			},
			{
				RelPath:        "src/crypto.ts",
				Lang:           models.LangTypeScript,
				Representation: models.RepSignature,
				Tokens:         40,
				Content:        "export function hashPassword(p: string): string",
			},
		},
	}
}

func TestParseCitationsExtractsPathAndLine(t *testing.T) {
	text := "See src/auth.ts:42 for the middleware, and src/crypto.ts for hashing."
	cs := audit.ParseCitations(text)
	if len(cs) != 2 {
		t.Fatalf("expected 2 citations, got %d: %+v", len(cs), cs)
	}
	if cs[0].RelPath != "src/auth.ts" || cs[0].Line != 42 {
		t.Errorf("bad first citation: %+v", cs[0])
	}
	if cs[1].RelPath != "src/crypto.ts" || cs[1].Line != 0 {
		t.Errorf("bad second citation: %+v", cs[1])
	}
}

func TestParseCitationsDeduplicates(t *testing.T) {
	text := "src/auth.ts:10 and again src/auth.ts:10 plus src/auth.ts"
	cs := audit.ParseCitations(text)
	if len(cs) != 2 {
		t.Fatalf("expected 2 unique citations (line 10 + bare), got %+v", cs)
	}
}

func TestValidateCitationsMarksValid(t *testing.T) {
	b := fixtureBundle()
	cs := audit.ParseCitations("The middleware is in src/auth.ts:3. Unrelated: foo/bar.ts")
	cs = audit.ValidateCitations(cs, b)

	var authC, fooC *audit.Citation
	for i := range cs {
		switch cs[i].RelPath {
		case "src/auth.ts":
			authC = &cs[i]
		case "foo/bar.ts":
			fooC = &cs[i]
		}
	}
	if authC == nil || !authC.Valid {
		t.Errorf("src/auth.ts should be valid, got %+v", authC)
	}
	if fooC == nil || fooC.Valid {
		t.Errorf("foo/bar.ts should be invalid, got %+v", fooC)
	}
	if fooC != nil && fooC.Reason == "" {
		t.Errorf("invalid citation should carry a reason")
	}
}

func TestValidateCitationsBasenameFallback(t *testing.T) {
	b := fixtureBundle()
	// Model refers to auth.ts without a directory — bundle has exactly one,
	// so the basename fallback should accept it and normalise the relpath.
	cs := audit.ParseCitations("check auth.ts")
	cs = audit.ValidateCitations(cs, b)
	if len(cs) != 1 || !cs[0].Valid || cs[0].RelPath != "src/auth.ts" {
		t.Errorf("expected basename fallback to normalise to src/auth.ts, got %+v", cs)
	}
}

func TestGroundedRatio(t *testing.T) {
	cs := []audit.Citation{{Valid: true}, {Valid: false}, {Valid: true}, {Valid: true}}
	if got := audit.GroundedRatio(cs); got != 0.75 {
		t.Errorf("grounded ratio 3/4 = 0.75, got %v", got)
	}
	if got := audit.GroundedRatio(nil); got != 1.0 {
		t.Errorf("no-citations default should be 1.0, got %v", got)
	}
}

func TestDetectDriftFindsUnknownSymbols(t *testing.T) {
	b := fixtureBundle()
	// verifyToken and hashPassword are in the bundle; AuthController and
	// sessionStore are not.
	resp := "Call verifyToken, then hashPassword. Internally it hits AuthController and session_store."
	d := audit.DetectDrift(resp, b)

	if !contains(d.UnknownSymbols, "AuthController") {
		t.Errorf("expected AuthController drift, got %+v", d.UnknownSymbols)
	}
	if !contains(d.UnknownSymbols, "session_store") {
		t.Errorf("expected session_store drift, got %+v", d.UnknownSymbols)
	}
	if contains(d.UnknownSymbols, "verifyToken") || contains(d.UnknownSymbols, "hashPassword") {
		t.Errorf("known symbols leaked into drift: %+v", d.UnknownSymbols)
	}
	if d.Rate <= 0 || d.Rate > 1 {
		t.Errorf("rate out of range: %v", d.Rate)
	}
}

func TestDetectDriftUnknownPath(t *testing.T) {
	b := fixtureBundle()
	d := audit.DetectDrift("see src/auth.ts and src/missing.ts", b)
	if !contains(d.UnknownPaths, "src/missing.ts") {
		t.Errorf("expected src/missing.ts in unknown paths: %+v", d.UnknownPaths)
	}
	if contains(d.UnknownPaths, "src/auth.ts") {
		t.Errorf("src/auth.ts wrongly flagged as drift")
	}
}

func TestScoreFactsRecall(t *testing.T) {
	hits, r := audit.ScoreFacts("We use jwt.sign and verify the token", []string{"jwt.sign", "hashPassword"})
	if len(hits) != 1 || hits[0] != "jwt.sign" {
		t.Errorf("unexpected hits: %+v", hits)
	}
	if r != 0.5 {
		t.Errorf("recall 1/2 expected, got %v", r)
	}
	if _, r := audit.ScoreFacts("anything", nil); r != 1.0 {
		t.Errorf("no facts → recall 1.0, got %v", r)
	}
}

func TestBundleHashStableAcrossFragmentOrder(t *testing.T) {
	b1 := fixtureBundle()
	b2 := fixtureBundle()
	b2.Fragments[0], b2.Fragments[1] = b2.Fragments[1], b2.Fragments[0]
	if audit.BundleHash(b1) != audit.BundleHash(b2) {
		t.Errorf("hash should be invariant under fragment order")
	}
}

func TestBundleHashChangesWithContent(t *testing.T) {
	b := fixtureBundle()
	h1 := audit.BundleHash(b)
	b.Fragments[0].Content += " // tweak"
	if audit.BundleHash(b) == h1 {
		t.Errorf("hash should change when content changes")
	}
}

func TestRunEndToEndWithStubModel(t *testing.T) {
	b := fixtureBundle()
	stub := audit.StubModel{
		Label: "stub-test",
		Response: "The middleware lives in src/auth.ts:1 and uses verifyToken. " +
			"Passwords are hashed in src/crypto.ts via hashPassword. " +
			"We also call GhostService, which does not exist.",
	}
	fixedNow := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)

	rec, err := audit.Run(context.Background(), stub, b, audit.Options{
		ExpectsFacts: []string{"verifyToken", "hashPassword", "never-mentioned"},
		Now:          func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if rec.Model != "stub-test" {
		t.Errorf("model id not recorded: %q", rec.Model)
	}
	if !rec.Timestamp.Equal(fixedNow) {
		t.Errorf("clock override ignored")
	}
	if rec.BundleHash == "" || len(rec.BundleHash) != 64 {
		t.Errorf("expected sha256 hex bundle hash, got %q", rec.BundleHash)
	}
	if len(rec.Citations) != 2 {
		t.Errorf("expected 2 citations, got %+v", rec.Citations)
	}
	if rec.GroundedRatio != 1.0 {
		t.Errorf("both citations valid → ratio 1.0, got %v", rec.GroundedRatio)
	}
	if !contains(rec.Drift.UnknownSymbols, "GhostService") {
		t.Errorf("GhostService should be flagged as drift: %+v", rec.Drift)
	}
	if rec.AnswerRecall != 2.0/3.0 {
		t.Errorf("recall 2/3 expected, got %v", rec.AnswerRecall)
	}
	// Replay guarantee: fragments carried forward verbatim.
	if len(rec.Fragments) != 2 || !strings.Contains(rec.Fragments[0].Content, "verifyToken") {
		t.Errorf("fragments not frozen correctly: %+v", rec.Fragments)
	}
}

// contains is a tiny string-slice helper to keep assertions readable.
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

