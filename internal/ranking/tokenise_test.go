package ranking

import (
	"reflect"
	"testing"
)

func TestSplitIdentifier(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// Headline cases from the dogfooding session.
		{"UpgradeWithSlack", []string{"Upgrade", "With", "Slack"}},
		{"fullCodeMaxTokens", []string{"full", "Code", "Max", "Tokens"}},
		{"RepExcerpt", []string{"Rep", "Excerpt"}},
		// Acronym followed by camel: split between the last upper of the
		// acronym and the start of the next word.
		{"MCPServer", []string{"MCP", "Server"}},
		{"URLParser", []string{"URL", "Parser"}},
		{"IOError", []string{"IO", "Error"}},
		// Numbers stay attached to a leading letter run; digit→letter boundary
		// still splits so the suffix doesn't swallow the next word.
		{"EvaluateG2Budget", []string{"Evaluate", "G2", "Budget"}},
		{"version2release3", []string{"version2", "release3"}},
		// Snake_case is decomposed too.
		{"upgrade_with_slack", []string{"upgrade", "with", "slack"}},
		{"_private", []string{"private"}},
		{"trailing_", []string{"trailing"}},
		// Already-flat words and pure acronyms have no boundary; return nil
		// so the caller knows there is nothing extra to add.
		{"auth", nil},
		{"MCP", nil},
		{"", nil},
		{"X", nil},
	}
	for _, c := range cases {
		got := splitIdentifier(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitIdentifier(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTokeniseSplitsCamelCase(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		// Headline regression: this was the failing case in the dogfooding
		// session — `upgrade` and `slack` were never produced, so the
		// excerpt extractor could not find UpgradeWithSlack as a symbol.
		// "with" is filtered as a stopword.
		{
			query: "How does UpgradeWithSlack affect pack density?",
			want:  []string{"upgradewithslack", "upgrade", "slack", "affect", "pack", "density"},
		},
		// PascalCase identifier alone produces both the flat token and the
		// split parts; "rep" survives the min-3 filter.
		{
			query: "RepExcerpt",
			want:  []string{"repexcerpt", "rep", "excerpt"},
		},
		// Acronym + camel tail: both pieces kept; flat form too.
		{
			query: "configure MCPServer",
			want:  []string{"configure", "mcpserver", "mcp", "server"},
		},
		// Letter+digit identifier: G2 (length 2) is dropped by the existing
		// min-3 filter — confirming this PR does NOT change the floor.
		{
			query: "EvaluateG2Budget",
			want:  []string{"evaluateg2budget", "evaluate", "budget"},
		},
		// Plain English query unchanged: no PascalCase, no extra tokens.
		{
			query: "how is authentication configured",
			want:  []string{"authentication", "configured"},
		},
	}
	for _, c := range cases {
		got := Tokenise(c.query)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokenise(%q)\n  got  = %v\n  want = %v", c.query, got, c.want)
		}
	}
}

func TestTokeniseDeduplicates(t *testing.T) {
	// "tests" raw → "tests"; splitIdentifier("tests") returns nil.
	// Then "Test" (in "TestRunner") → "test" via Tokenise's lowercase. The
	// dedupe map must keep both as distinct entries since "tests" != "test"
	// after stemming is the ranker's concern, not the tokeniser's.
	got := Tokenise("tests TestRunner Test")
	want := []string{"tests", "testrunner", "test", "runner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokenise dedup\n  got  = %v\n  want = %v", got, want)
	}
}
