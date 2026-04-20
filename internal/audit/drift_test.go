package audit

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

// driftBundle is a fixture that mirrors a small but realistic Go/TS bundle.
// Kept in-package so the tests can exercise the unexported classifier
// helpers directly instead of replicating their rules.
func driftBundle() models.Bundle {
	return models.Bundle{
		Query: "how does authentication work",
		Fragments: []models.ContextFragment{
			{
				RelPath:        "src/auth.ts",
				Lang:           models.LangTypeScript,
				Representation: models.RepFullCode,
				Content: `import { verify, sign } from "jsonwebtoken";
export function verifyToken(t: string) { return jwt.verify(t, SECRET); }
export function issueToken(p: Payload) { return jwt.sign(p, SECRET); }
export const AuthMiddleware = () => {};`,
			},
			{
				RelPath:        "src/crypto.ts",
				Lang:           models.LangTypeScript,
				Representation: models.RepSignature,
				Content:        "export function hashPassword(p: string): string;",
			},
			{
				RelPath:        "src/session.go",
				Lang:           models.LangGo,
				Representation: models.RepFullCode,
				Content:        "func openSession() *Session { return nil }\nvar session_timeout = 30",
			},
		},
	}
}

// TestClassifyTokenMatrix pins down the single-token classifier. Keep this
// close to a truth table — if we add a new shape rule, extend the matrix;
// otherwise any regression will flip a row.
func TestClassifyTokenMatrix(t *testing.T) {
	cases := []struct {
		tok  string
		want tokenKind
	}{
		// Narrative / sentence starters — must never be codey.
		{"This", kindSkip},
		{"Here", kindSkip},
		{"When", kindSkip},
		{"Overall", kindSkip},
		{"However", kindSkip},
		{"Usually", kindSkip},
		{"Typically", kindSkip},
		{"Importantly", kindSkip},
		{"Although", kindSkip},
		{"Note", kindSkip},   // prose "Note: ..."
		{"Useful", kindSkip}, // prose adjective
		{"Bundle", kindSkip}, // deliberate false negative: single Capital
		{"Router", kindSkip}, // same: accept as trade-off
		// Short prose.
		{"We", kindSkip},
		{"It", kindSkip},
		{"the", kindSkip},
		{"and", kindSkip},
		{"foo", kindSkip},
		// Code identifiers — must be kept.
		{"AuthController", kindSymbol},
		{"GhostService", kindSymbol},
		{"session_store", kindSymbol},
		{"verify_token", kindSymbol},
		{"hashPassword", kindSymbol},
		{"verifyToken", kindSymbol},
		{"HTTPServer", kindSymbol},
		{"HTTP", kindSymbol},
		{"JSON", kindSymbol},
		{"MAX_RETRIES", kindSymbol},
		// API-like dotted names.
		{"jwt.sign", kindAPI},
		{"os.path.join", kindAPI},
		{"config.Foo", kindAPI},
		// Prose dotted artefacts must not become APIs.
		{"e.g", kindSkip},
		{"i.e", kindSkip},
		// Paths via extension / slash.
		{"utils.py", kindPath},
		{"src/auth.ts", kindPath},
		{"docs/architecture.md", kindPath},
	}

	for _, c := range cases {
		t.Run(c.tok, func(t *testing.T) {
			if got := classifyToken(c.tok); got != c.want {
				t.Errorf("classifyToken(%q) = %v, want %v", c.tok, got, c.want)
			}
		})
	}
}

// TestDetectDriftIgnoresPureNarrative is the headline test: a response made
// of only English prose should produce zero unknown symbols. If this breaks
// the detector will spam users with "drift" on every capitalised sentence.
func TestDetectDriftIgnoresPureNarrative(t *testing.T) {
	b := driftBundle()
	resp := `This is an overview of how the code works. Here are the main pieces.
Overall, the design is simple. Usually the flow goes through a middleware.
However, when something goes wrong, you see it in the logs. Note that
callers should not rely on the error message string.`

	d := DetectDrift(resp, b)
	if len(d.UnknownSymbols) > 0 {
		t.Errorf("pure narrative produced %d unknown symbols: %v", len(d.UnknownSymbols), d.UnknownSymbols)
	}
	if len(d.UnknownAPIs) > 0 {
		t.Errorf("pure narrative produced unknown APIs: %v", d.UnknownAPIs)
	}
	if len(d.UnknownPaths) > 0 {
		t.Errorf("pure narrative produced unknown paths: %v", d.UnknownPaths)
	}
}

// TestDetectDriftCatchesRealSymbols makes sure the tightening did not
// silence real hallucinations. Pair each fake token with realistic prose
// around it so the scenario matches something Claude might actually emit.
func TestDetectDriftCatchesRealSymbols(t *testing.T) {
	b := driftBundle()
	resp := `The flow goes through AuthMiddleware and then calls verifyToken.
But it also uses FakeLoginService and session_expiry, which do not exist.`

	d := DetectDrift(resp, b)
	if !contains2(d.UnknownSymbols, "FakeLoginService") {
		t.Errorf("expected FakeLoginService in drift, got %v", d.UnknownSymbols)
	}
	if !contains2(d.UnknownSymbols, "session_expiry") {
		t.Errorf("expected session_expiry in drift, got %v", d.UnknownSymbols)
	}
	if contains2(d.UnknownSymbols, "AuthMiddleware") {
		t.Errorf("AuthMiddleware is in bundle; must not be flagged")
	}
	if contains2(d.UnknownSymbols, "verifyToken") {
		t.Errorf("verifyToken is in bundle; must not be flagged")
	}
}

// TestDetectDriftAPIBucket verifies the dotted-name split: jwt.sign comes
// from the bundle (it appears as `jwt.verify` and `jwt.sign` in the import
// plus call), jwt.rotate does not.
func TestDetectDriftAPIBucket(t *testing.T) {
	b := driftBundle()
	resp := "We call jwt.sign and then jwt.rotate to refresh the key."

	d := DetectDrift(resp, b)
	if !contains2(d.UnknownAPIs, "jwt.rotate") {
		t.Errorf("expected jwt.rotate in unknown APIs, got %v", d.UnknownAPIs)
	}
	if contains2(d.UnknownAPIs, "jwt.sign") {
		t.Errorf("jwt.sign is in bundle content; must not be flagged: %v", d.UnknownAPIs)
	}
}

// TestDetectDriftPathBucket re-verifies the path check still lives in the
// path bucket after the refactor.
func TestDetectDriftPathBucket(t *testing.T) {
	b := driftBundle()
	resp := "Look at src/auth.ts and src/missing/handler.ts for details."
	d := DetectDrift(resp, b)

	if !contains2(d.UnknownPaths, "src/missing/handler.ts") {
		t.Errorf("expected missing path flagged, got %v", d.UnknownPaths)
	}
	if contains2(d.UnknownPaths, "src/auth.ts") {
		t.Errorf("src/auth.ts is in bundle; must not be flagged")
	}
}

// TestDetectDriftMixedRealistic exercises a Claude-style multi-paragraph
// answer with a mix of narrative, real code refs, one fake service, one
// fake path, and one fake API call. Numbers chosen so the rate stays
// interpretable (< 100%) and each bucket has at least one entry.
func TestDetectDriftMixedRealistic(t *testing.T) {
	b := driftBundle()
	resp := `The authentication flow is straightforward. Here is the breakdown:

1. AuthMiddleware runs first (src/auth.ts:3). It delegates to verifyToken,
   which calls jwt.verify under the hood.
2. Passwords are hashed via hashPassword in src/crypto.ts.
3. Session lifecycle goes through openSession; failures bubble up.

However, some parts are delegated to PhantomAuthorizer (I cannot see it in
the bundle) and the ghost path internal/fake/module.go. There is also a
call to redis.flushDB which seems unrelated.`

	d := DetectDrift(resp, b)

	// Narrative words must be absent.
	for _, narr := range []string{"This", "Here", "However", "The", "Overall", "Usually"} {
		if contains2(d.UnknownSymbols, narr) {
			t.Errorf("narrative %q leaked into symbols", narr)
		}
	}
	// Real drift must be present.
	if !contains2(d.UnknownSymbols, "PhantomAuthorizer") {
		t.Errorf("PhantomAuthorizer should be flagged: %v", d.UnknownSymbols)
	}
	if !contains2(d.UnknownPaths, "internal/fake/module.go") {
		t.Errorf("internal/fake/module.go should be flagged: %v", d.UnknownPaths)
	}
	if !contains2(d.UnknownAPIs, "redis.flushDB") {
		t.Errorf("redis.flushDB should be flagged: %v", d.UnknownAPIs)
	}
	// Rate must be interpretable.
	if d.Rate <= 0 || d.Rate >= 1 {
		t.Errorf("mixed response should yield a non-trivial rate, got %v", d.Rate)
	}
	// Known refs must have raised KnownCount.
	if d.KnownCount == 0 {
		t.Errorf("real refs should have populated KnownCount, got 0")
	}
}

// TestDetectDriftEmptyResponseIsZero locks in the zero-cost behaviour: no
// response text, no claims, no drift. The replay workflow needs this to
// stay honest when the user pastes an empty file by mistake.
func TestDetectDriftEmptyResponseIsZero(t *testing.T) {
	for _, s := range []string{"", "   \n\t  "} {
		d := DetectDrift(s, driftBundle())
		if d.UnknownCount != 0 || d.KnownCount != 0 || d.Rate != 0 {
			t.Errorf("empty response should produce zero report, got %+v on %q", d, s)
		}
	}
}

// TestDetectDriftRatePrecision checks the rate computation against a hand-
// counted scenario: 3 real refs (auth.ts, verifyToken, jwt.sign) and 3 fake
// ones (one of each bucket) → rate = 3/6 = 0.5.
func TestDetectDriftRatePrecision(t *testing.T) {
	b := driftBundle()
	resp := strings.Join([]string{
		"See src/auth.ts.",                   // path known
		"Call verifyToken in that file.",     // symbol known
		"Under the hood it uses jwt.sign.",   // api known
		"There is also a fake GhostService.", // symbol unknown
		"And a fake path src/ghost.ts.",      // path unknown
		"Plus the dotted call redis.flushDB.", // api unknown
	}, " ")
	d := DetectDrift(resp, b)

	if d.KnownCount != 3 {
		t.Errorf("expected 3 known refs, got %d (%+v)", d.KnownCount, d)
	}
	if d.UnknownCount != 3 {
		t.Errorf("expected 3 unknown refs, got %d (%+v)", d.UnknownCount, d)
	}
	if d.Rate != 0.5 {
		t.Errorf("expected rate 0.5, got %v", d.Rate)
	}
}

// contains2 is a tiny slice-contains helper. Named to avoid shadowing the
// one in audit_test.go (different package: _test vs in-package here).
func contains2(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
