package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func TestResolveRepoReturnsAbsolutePath(t *testing.T) {
	got, err := resolveRepo(".")
	if err != nil {
		t.Fatalf("resolve repo: %v", err)
	}
	want, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	if got != want {
		t.Fatalf("resolveRepo(.) = %q, want %q", got, want)
	}
}

// ---------- string / identifier helpers ----------

func TestSplitIdentifierForSearch(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"snake_case", "parse_chunks", []string{"parse", "chunks"}},
		{"dotted", "foo.bar.baz", []string{"foo", "bar", "baz"}},
		{"dashed", "XML-Parser", []string{"xml", "parser"}},
		{"camelCase_not_split", "handleSearchQuery", []string{"handlesearchquery"}},
		{"short_filtered", "a.b.cd", nil},
		{"empty", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitIdentifierForSearch(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("splitIdentifierForSearch(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestStripExt(t *testing.T) {
	cases := map[string]string{
		"foo.go":            "foo",
		"path/to/file.tsx":  "path/to/file",
		"no_extension":      "no_extension",
		"multi.dot.name.py": "multi.dot.name",
		"":                  "",
	}
	for in, want := range cases {
		if got := stripExt(in); got != want {
			t.Errorf("stripExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExactSearchTerms(t *testing.T) {
	t.Run("basic_lower_dedupe", func(t *testing.T) {
		got := exactSearchTerms([]string{"Foo", "bar", "FOO", "  bar  "})
		want := []string{"foo", "bar"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("short_filtered", func(t *testing.T) {
		got := exactSearchTerms([]string{"hi", "ok", "yes"})
		want := []string{"yes"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("cap_at_maxPatterns", func(t *testing.T) {
		in := make([]string, 15)
		for i := range in {
			in[i] = "term" + string(rune('a'+i))
		}
		got := exactSearchTerms(in)
		if len(got) != 12 {
			t.Errorf("expected len 12 (cap), got %d", len(got))
		}
	})
}

func TestNormalizeRGPath(t *testing.T) {
	repo := "/abs/repo"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"relative", "internal/foo.go", "internal/foo.go"},
		{"absolute_under_repo", "/abs/repo/internal/foo.go", "internal/foo.go"},
		{"dot_slash_prefix", "./foo.go", "foo.go"},
		{"empty", "", ""},
		{"whitespace_only", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeRGPath(repo, c.in)
			if got != c.want {
				t.Errorf("normalizeRGPath(%q, %q) = %q, want %q", repo, c.in, got, c.want)
			}
		})
	}
}

func TestFilenameMatchesExactTerm(t *testing.T) {
	cases := []struct {
		name    string
		relPath string
		terms   []string
		want    bool
	}{
		{"basename_match", "internal/foo.go", []string{"foo.go"}, true},
		{"stem_match", "internal/foo.go", []string{"foo"}, true},
		{"identifier_part_match", "internal/foo_bar.go", []string{"bar"}, true},
		{"no_match", "internal/foo.go", []string{"baz"}, false},
		{"empty_terms", "internal/foo.go", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			termSet := make(map[string]bool, len(c.terms))
			for _, term := range c.terms {
				termSet[strings.ToLower(strings.TrimSpace(term))] = true
			}
			got := filenameMatchesExactTerm(c.relPath, termSet)
			if got != c.want {
				t.Errorf("filenameMatchesExactTerm(%q, %v) = %v, want %v", c.relPath, c.terms, got, c.want)
			}
		})
	}
}

func TestChangedPathSet(t *testing.T) {
	got := changedPathSet([]string{"foo/bar.go", "  ", "internal/x.go"})
	want := map[string]bool{"foo/bar.go": true, "internal/x.go": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changedPathSet = %v, want %v", got, want)
	}

	if got := changedPathSet(nil); got != nil {
		t.Errorf("changedPathSet(nil) = %v, want nil", got)
	}
	if got := changedPathSet([]string{}); got != nil {
		t.Errorf("changedPathSet(empty) = %v, want nil", got)
	}
}

// ---------- scoring ----------

func TestScoreChunkHit(t *testing.T) {
	rec := models.FileRecord{RelPath: "internal/parser/parser.go"}
	chunk := models.Chunk{Symbol: "ParseFunction", Kind: "func"}
	snippet := "func ParseFunction(input string) ast.Node { return parseFunction(input) }"
	terms := []string{"parsefunction", "parser", "func"}

	score, reasons := scoreChunkHit(rec, chunk, snippet, terms)

	if score <= 0 {
		t.Fatalf("expected positive score, got %v", score)
	}
	wantReasons := []string{"symbol_match", "path_match", "kind_match", "content_match", "chunk_scope"}
	for _, want := range wantReasons {
		if !containsString(reasons, want) {
			t.Errorf("missing reason %q in %v", want, reasons)
		}
	}

	// Unrelated query yields no score and no reasons.
	noscore, noreasons := scoreChunkHit(rec, chunk, snippet, []string{"zzzzzz"})
	if noscore != 0 || len(noreasons) != 0 {
		t.Errorf("expected zero score for unrelated terms, got %v / %v", noscore, noreasons)
	}
}

func TestSemanticBoost(t *testing.T) {
	if got := semanticBoost(0.0); got != 1.0 {
		t.Errorf("below-threshold sim=0 → boost %v, want 1.0", got)
	}
	if got := semanticBoost(0.18); got != 1.0 {
		t.Errorf("at-threshold sim=0.18 → boost %v, want 1.0", got)
	}
	if got := semanticBoost(0.5); !(got > 1.0 && got < 8.0) {
		t.Errorf("mid sim → boost %v, want in (1, 8)", got)
	}
	// sim clamps to 1; with threshold 0.18 and slope 8, max attainable boost is 7.56.
	if got := semanticBoost(2.0); !(got > 7.0 && got <= 8.0) {
		t.Errorf("above-1 sim → boost %v, want near max", got)
	}
}

func TestSymbolScore(t *testing.T) {
	if got := symbolScore("ParseFunction", []string{"parsefunction"}); got != 18.0 {
		t.Errorf("exact-match symbolScore = %v, want 18", got)
	}
	if got := symbolScore("ParseFunction", []string{"unrelatedterm"}); got != 3.0 {
		t.Errorf("non-match symbolScore = %v, want 3", got)
	}
}

func TestTextMatchesTerms(t *testing.T) {
	if !textMatchesTerms("ParseFunction", []string{"parse"}) {
		t.Errorf("expected match: ParseFunction contains parse")
	}
	if textMatchesTerms("", []string{"anything"}) {
		t.Errorf("empty text should not match")
	}
	if textMatchesTerms("text", nil) {
		t.Errorf("nil terms should not match")
	}
}

// ---------- candidate boosts / penalties ----------

func TestAddReasonAndPenalty(t *testing.T) {
	h := &Hit{Score: 0}
	addReason(h, "first", 2.5)
	if h.Score != 2.5 || !containsString(h.Reasons, "first") {
		t.Fatalf("after addReason: score=%v reasons=%v", h.Score, h.Reasons)
	}
	addReason(h, "first", 1.0) // duplicate reason, score still accumulates
	if h.Score != 3.5 {
		t.Errorf("duplicate reason should still add score; got %v", h.Score)
	}
	if len(h.Reasons) != 1 {
		t.Errorf("duplicate reason should not appear twice; got %v", h.Reasons)
	}

	addPenalty(h, "pen", 2.0)
	if h.Score != 1.5 || !containsString(h.Reasons, "pen") {
		t.Errorf("after addPenalty: score=%v reasons=%v", h.Score, h.Reasons)
	}

	h2 := &Hit{Score: 1.0}
	addPenalty(h2, "huge", 99.0)
	if h2.Score != 0 {
		t.Errorf("penalty should floor at 0, got %v", h2.Score)
	}
}

func TestApplyExactBoost(t *testing.T) {
	cands := []candidate{
		{filePath: "/repo/a.go", hit: Hit{Path: "a.go", StartLine: 10, EndLine: 20}},
		{filePath: "/repo/b.go", hit: Hit{Path: "b.go", StartLine: 5, EndLine: 15}},
	}
	signals := map[string]exactSignal{
		"a.go": {filename: true, lines: map[int]bool{15: true}}, // overlaps [10,20]
		"b.go": {lines: map[int]bool{100: true}},                // no overlap
	}
	applyExactBoost(cands, signals)

	if !containsString(cands[0].hit.Reasons, "exact_filename") {
		t.Errorf("expected exact_filename on a.go, got %v", cands[0].hit.Reasons)
	}
	if !containsString(cands[0].hit.Reasons, "exact_content") {
		t.Errorf("expected exact_content on a.go (line 15 in [10,20]), got %v", cands[0].hit.Reasons)
	}
	if containsString(cands[1].hit.Reasons, "exact_content") {
		t.Errorf("did not expect exact_content on b.go, got %v", cands[1].hit.Reasons)
	}
	if cands[0].hit.Score <= 0 {
		t.Errorf("expected positive score on a.go after boosts, got %v", cands[0].hit.Score)
	}
}

func TestApplyLongChunkPenalty(t *testing.T) {
	cands := []candidate{
		{hit: Hit{Path: "a.go", Score: 5.0, TokenEstimate: 100}}, // smallest
		{hit: Hit{Path: "b.go", Score: 5.0, TokenEstimate: 450}}, // below threshold
		{hit: Hit{Path: "d.go", Score: 5.0, TokenEstimate: 800}}, // above threshold and >= 2x smallest
	}
	applyLongChunkPenalty(cands)

	if containsString(cands[0].hit.Reasons, "long_chunk_penalty") {
		t.Errorf("smallest chunk should not be penalized")
	}
	if containsString(cands[1].hit.Reasons, "long_chunk_penalty") {
		t.Errorf("below-threshold (450 < 500) should not be penalized")
	}
	if !containsString(cands[2].hit.Reasons, "long_chunk_penalty") {
		t.Errorf("800 tokens should be penalized")
	}
	if cands[2].hit.Score >= 5.0 {
		t.Errorf("d.go score should be reduced, got %v", cands[2].hit.Score)
	}
}

func TestApplyWorkingSetBoost(t *testing.T) {
	cands := []candidate{
		{filePath: "/repo/a.go", hit: Hit{Path: "a.go", Score: 5.0, ContentHash: "h1", StartLine: 1}},
		{filePath: "/repo/b.go", hit: Hit{Path: "b.go", Score: 5.0, ContentHash: "h2", StartLine: 1}},
		{filePath: "/repo/c.go", hit: Hit{Path: "c.go", Score: 0, ContentHash: "h3", StartLine: 1}},
	}
	changed := map[string]bool{"a.go": true, "c.go": true}
	applyWorkingSetBoost(cands, changed)

	if !containsString(cands[0].hit.Reasons, "working_set") {
		t.Errorf("expected working_set on a.go (scoring + changed)")
	}
	if containsString(cands[1].hit.Reasons, "working_set") {
		t.Errorf("did not expect working_set on b.go (not changed)")
	}
	if !containsString(cands[2].hit.Reasons, "working_set") {
		t.Errorf("expected working_set on c.go (bridge: zero-score + changed)")
	}
}

func TestSelectWorkingSetBridgeCandidates(t *testing.T) {
	cands := []candidate{
		{filePath: "/repo/a.go", hit: Hit{Path: "a.go", Score: 5.0, ContentHash: "h1"}}, // scoring → not bridge
		{filePath: "/repo/b.go", hit: Hit{Path: "b.go", Score: 0, ContentHash: "h2"}},   // bridge
		{filePath: "/repo/c.go", hit: Hit{Path: "c.go", Score: 0, ContentHash: "h3"}},   // not changed → not bridge
	}
	changed := map[string]bool{"a.go": true, "b.go": true}
	selected := selectWorkingSetBridgeCandidates(cands, changed)
	if len(selected) != 1 {
		t.Errorf("expected exactly 1 bridge, got %d (%v)", len(selected), selected)
	}
	if !selected[candidateKey(cands[1])] {
		t.Errorf("expected b.go selected as bridge, got %v", selected)
	}
}

func TestApplyGraphBoost(t *testing.T) {
	cands := []candidate{
		{filePath: "/repo/a.go", hit: Hit{Path: "a.go", Score: 5.0, ContentHash: "h1", StartLine: 1}},
		{filePath: "/repo/b.go", hit: Hit{Path: "b.go", Score: 0, ContentHash: "h2", StartLine: 1}},
		{filePath: "/repo/c.go", hit: Hit{Path: "c.go", Score: 5.0, ContentHash: "h3", StartLine: 1}},
	}
	relations := []models.FileRelation{
		{SourcePath: "/repo/a.go", TargetPath: "/repo/b.go", RelType: "import"},
	}
	applyGraphBoost(cands, relations)

	if !containsString(cands[1].hit.Reasons, "graph_dependency") {
		t.Errorf("expected graph_dependency on b.go (bridge via a→b), got %v", cands[1].hit.Reasons)
	}
	if containsString(cands[2].hit.Reasons, "graph_dependency") {
		t.Errorf("c.go has no relation, should not receive graph boost")
	}
}

func TestSeedPathsForCandidates(t *testing.T) {
	cands := []candidate{
		{filePath: "/repo/a.go", hit: Hit{Path: "a.go", Score: 1.0}},
		{filePath: "/repo/b.go", hit: Hit{Path: "b.go", Score: 0}}, // skipped (no score)
		{filePath: "/repo/c.go", hit: Hit{Path: "c.go", Score: 3.0}},
	}
	got := seedPathsForCandidates(cands, 8)
	if len(got) != 2 || !got["/repo/a.go"] || !got["/repo/c.go"] {
		t.Errorf("expected {a.go, c.go} seeds, got %v", got)
	}

	got1 := seedPathsForCandidates(cands, 1)
	if len(got1) != 1 || !got1["/repo/c.go"] {
		t.Errorf("limit=1 should keep highest-score seed (c.go); got %v", got1)
	}
}

// ---------- snippet helpers ----------

func TestLinesOverlap(t *testing.T) {
	lines := map[int]bool{15: true, 50: true}
	if !linesOverlap(lines, 10, 20) {
		t.Errorf("expected overlap with [10,20] (15 inside)")
	}
	if !linesOverlap(lines, 50, 50) {
		t.Errorf("expected overlap with exact line 50")
	}
	if linesOverlap(lines, 100, 200) {
		t.Errorf("did not expect overlap with [100,200]")
	}
	if linesOverlap(nil, 1, 100) {
		t.Errorf("nil lines should not overlap")
	}
}

func TestSnippetForRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"
	if got := snippetForRange(content, 2, 4); got != "line2\nline3\nline4" {
		t.Errorf("snippet(2,4) = %q", got)
	}
	if got := snippetForRange(content, 1, 1); got != "line1" {
		t.Errorf("snippet(1,1) = %q", got)
	}
	if got := snippetForRange(content, 0, 100); got != "line1\nline2\nline3\nline4\nline5" {
		t.Errorf("snippet clamps out-of-range: %q", got)
	}
	if got := snippetForRange("", 1, 1); got != "" {
		t.Errorf("empty content → empty snippet; got %q", got)
	}
}

func TestApplyTestPenalty(t *testing.T) {
	t.Run("downranks test files when no test intent", func(t *testing.T) {
		cands := []candidate{
			{hit: Hit{Path: "src/auth.go", Score: 10.0}},
			{hit: Hit{Path: "src/auth_test.go", Score: 10.0}},
		}
		applyTestPenalty(cands, "how does authentication work?")

		if cands[0].hit.Score != 10.0 {
			t.Errorf("production file should not be penalised, got %v", cands[0].hit.Score)
		}
		if cands[1].hit.Score >= 10.0 {
			t.Errorf("test file should be penalised, got %v", cands[1].hit.Score)
		}
		if !containsString(cands[1].hit.Reasons, "test_like_downrank") {
			t.Errorf("expected test_like_downrank reason, got %v", cands[1].hit.Reasons)
		}
	})

	t.Run("preserves test files when test intent detected", func(t *testing.T) {
		cands := []candidate{
			{hit: Hit{Path: "src/auth.go", Score: 10.0}},
			{hit: Hit{Path: "src/auth_test.go", Score: 10.0}},
		}
		applyTestPenalty(cands, "run the unit tests for auth")

		if cands[0].hit.Score != 10.0 {
			t.Errorf("production file should not be penalised, got %v", cands[0].hit.Score)
		}
		if cands[1].hit.Score != 10.0 {
			t.Errorf("test file should not be penalised under test intent, got %v", cands[1].hit.Score)
		}
		if !containsString(cands[1].hit.Reasons, "query_test_intent_detected") {
			t.Errorf("expected query_test_intent_detected reason, got %v", cands[1].hit.Reasons)
		}
		if containsString(cands[1].hit.Reasons, "test_like_downrank") {
			t.Errorf("should not downrank under test intent, got reasons %v", cands[1].hit.Reasons)
		}
	})
}

// ---------- end-to-end integration ----------

func TestSearchEndToEnd(t *testing.T) {
	// Force deterministic mock embeddings independently of dev env.
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")

	repo := t.TempDir()
	write := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(repo, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
	}
	write("parser.go", "package main\n\nfunc ParseFunction(input string) string {\n\treturn input\n}\n")
	write("ranking.go", "package main\n\nfunc RankResults(items []string) []string {\n\treturn items\n}\n")
	write("README.md", "# Test repo\n")

	cfg, err := config.New(repo)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		db.Close()
		t.Fatalf("indexer.Run: %v", err)
	}
	db.Close() // Search opens its own handle

	resp, err := Search(context.Background(), Options{
		Query: "ParseFunction",
		Repo:  repo,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected at least one result")
	}

	top := resp.Results[0]
	if filepath.Base(top.Path) != "parser.go" {
		t.Errorf("expected parser.go on top, got %s (results=%v)", filepath.Base(top.Path), resp.Results)
	}
	if top.Score <= 0 {
		t.Errorf("expected positive score on top hit, got %v", top.Score)
	}
	if len(top.Reasons) == 0 {
		t.Errorf("expected reasons populated on top hit, got empty")
	}
}

func TestSymbolExactlyNamed(t *testing.T) {
	tests := []struct {
		symbol string
		terms  []string
		want   bool
	}{
		// Raw lowercased identifier token equals the full symbol.
		{"UpgradeWithSlack", []string{"upgradewithslack", "packager"}, true},
		// Term equals the last dotted component (method name).
		{"CliRunner.invoke", []string{"invoke", "commands"}, true},
		// Term equals a class symbol exactly.
		{"Context", []string{"context", "object"}, true},
		// Substring is NOT enough — that's symbol_match's job.
		{"ContextManager", []string{"context"}, false},
		{"make_context", []string{"context"}, false},
		// Middle dotted components don't count.
		{"Context.scope", []string{"context"}, false},
		{"", []string{"anything"}, false},
		{"Open", nil, false},
	}
	for _, tc := range tests {
		if got := symbolExactlyNamed(tc.symbol, tc.terms); got != tc.want {
			t.Errorf("symbolExactlyNamed(%q, %v) = %t, want %t", tc.symbol, tc.terms, got, tc.want)
		}
	}
}
