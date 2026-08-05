package retrieval

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/storage"
)

// defaultTestWeights returns a fresh default weight set for direct calls to
// the scoring helpers under test.
func defaultTestWeights() *Weights {
	w := DefaultWeights()
	return &w
}

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

func TestExactSignalCacheIsBounded(t *testing.T) {
	session := &Session{exactCache: make(map[string]map[string]exactSignal)}
	for i := 0; i < maxExactCacheLen; i++ {
		session.cacheExactSignals(string(rune(i)), map[string]exactSignal{})
	}
	if len(session.exactCache) != maxExactCacheLen {
		t.Fatalf("cache size = %d, want %d before rollover", len(session.exactCache), maxExactCacheLen)
	}

	session.cacheExactSignals("rollover", map[string]exactSignal{})
	if len(session.exactCache) != 1 {
		t.Fatalf("cache size = %d after rollover, want 1", len(session.exactCache))
	}
	if _, ok := session.exactCache["rollover"]; !ok {
		t.Fatal("new cache entry missing after rollover")
	}
}

// ---------- scoring ----------

func TestScoreChunkHit(t *testing.T) {
	rec := models.FileRecord{RelPath: "internal/parser/parser.go"}
	chunk := models.Chunk{Symbol: "ParseFunction", Kind: "func"}
	snippet := "func ParseFunction(input string) ast.Node { return parseFunction(input) }"
	terms := []string{"parsefunction", "parser", "func"}

	score, reasons := scoreChunkHit(rec, chunk, snippet, terms, defaultTestWeights())

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
	noscore, noreasons := scoreChunkHit(rec, chunk, snippet, []string{"zzzzzz"}, defaultTestWeights())
	if noscore != 0 || len(noreasons) != 0 {
		t.Errorf("expected zero score for unrelated terms, got %v / %v", noscore, noreasons)
	}
}

func TestSemanticBoost(t *testing.T) {
	if got := semanticBoost(0.0, defaultTestWeights()); got != 1.0 {
		t.Errorf("below-threshold sim=0 → boost %v, want 1.0", got)
	}
	if got := semanticBoost(0.18, defaultTestWeights()); got != 1.0 {
		t.Errorf("at-threshold sim=0.18 → boost %v, want 1.0", got)
	}
	if got := semanticBoost(0.5, defaultTestWeights()); !(got > 1.0 && got < 8.0) {
		t.Errorf("mid sim → boost %v, want in (1, 8)", got)
	}
	// sim clamps to 1; with threshold 0.18 and slope 8, max attainable boost is 7.56.
	if got := semanticBoost(2.0, defaultTestWeights()); !(got > 7.0 && got <= 8.0) {
		t.Errorf("above-1 sim → boost %v, want near max", got)
	}
}

func TestSymbolScore(t *testing.T) {
	if got := symbolScore("ParseFunction", []string{"parsefunction"}, defaultTestWeights()); got != 18.0 {
		t.Errorf("exact-match symbolScore = %v, want 18", got)
	}
	if got := symbolScore("ParseFunction", []string{"unrelatedterm"}, defaultTestWeights()); got != 3.0 {
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
	applyExactBoost(cands, signals, defaultTestWeights())

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
	applyLongChunkPenalty(cands, defaultTestWeights())

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
	applyWorkingSetBoost(cands, changed, defaultTestWeights())

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
	applyGraphBoost(cands, relations, defaultTestWeights())

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
		applyTestPenalty(cands, "how does authentication work?", defaultTestWeights())

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
		applyTestPenalty(cands, "run the unit tests for auth", defaultTestWeights())

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
		_ = db.Close()
		t.Fatalf("indexer.Run: %v", err)
	}
	if err := db.Close(); err != nil { // Search opens its own handle.
		t.Fatalf("close index: %v", err)
	}

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

func TestSessionSearchRejectsContentChangedAfterSnapshot(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	path := filepath.Join(repo, "service.go")
	original := "package service\n\nfunc Original() string {\n\treturn \"old\"\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	replacement := "package service\n\nfunc Replacement() string {\n\treturn \"new\"\n}\n"
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}

	response, err := session.Search(context.Background(), Options{
		Query: "Replacement",
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf(
			"changed source must not be paired with stale indexed ranges/hashes: %+v",
			response.Results,
		)
	}
}

func TestSessionExactContentUsesCachedGenerationAfterEdit(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	path := filepath.Join(repo, "service.go")
	original := "package service\n\nfunc Stable() string {\n\treturn \"legacytoken\"\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.Search(context.Background(), Options{
		Query: "legacytoken",
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) == 0 {
		t.Fatal("initial search did not populate the session snapshot")
	}
	if _, ok := session.contentCache[path]; !ok {
		t.Fatal("initial search did not cache the indexed file generation")
	}

	replacement := "package service\n\nfunc Stable() string {\n\treturn \"brandnewmarker\"\n}\n"
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}

	currentTerm, err := session.Search(context.Background(), Options{
		Query: "brandnewmarker",
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range currentTerm.Results {
		if containsString(hit.Reasons, "exact_content") {
			t.Fatalf("live-tree exact signal was attached to a cached chunk: %+v", hit)
		}
	}
	if len(currentTerm.Results) != 0 {
		t.Fatalf("new bytes must not rank the session's old snapshot: %+v", currentTerm.Results)
	}

	cachedTerm, err := session.Search(context.Background(), Options{
		Query: "legacytoken",
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cachedTerm.Results) == 0 {
		t.Fatal("session did not retain its coherent cached generation")
	}
	top := cachedTerm.Results[0]
	if !strings.Contains(top.Snippet, "legacytoken") ||
		strings.Contains(top.Snippet, "brandnewmarker") {
		t.Fatalf("cached result mixed file generations: %+v", top)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(top.Snippet)))
	if top.ContentHash != wantHash {
		t.Fatalf("cached content hash = %q, want %q for snippet %q", top.ContentHash, wantHash, top.Snippet)
	}
}

func TestNewSessionReindexesChangedContentBeforeSearch(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	path := filepath.Join(repo, "service.go")
	original := "package service\n\nfunc Original() string {\n\treturn \"old\"\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	replacement := "package service\n\nfunc Replacement() string {\n\treturn \"new\"\n}\n"
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	response, err := session.Search(context.Background(), Options{
		Query: "Replacement",
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	var replacementHit *Hit
	for i := range response.Results {
		if response.Results[i].Path == "service.go" &&
			response.Results[i].Symbol == "Replacement" {
			replacementHit = &response.Results[i]
			break
		}
	}
	if replacementHit == nil {
		t.Fatalf("freshly reindexed Replacement chunk not found: %+v", response.Results)
	}
	if !strings.Contains(replacementHit.Snippet, `return "new"`) ||
		strings.Contains(replacementHit.Snippet, "Original") {
		t.Fatalf("result does not contain the current coherent generation: %+v", replacementHit)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(replacementHit.Snippet)))
	if replacementHit.ContentHash != wantHash {
		t.Fatalf(
			"content hash %q does not describe returned snippet %q (want %q)",
			replacementHit.ContentHash,
			replacementHit.Snippet,
			wantHash,
		)
	}
}

func TestNewSessionRebuildsStaleEmbeddingProvider(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	path := filepath.Join(repo, "stable.go")
	if err := os.WriteFile(
		path,
		[]byte("package stable\n\nfunc Current() string { return \"current\" }\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.UpdateChunks(path, []models.Chunk{{
		FilePath:      path,
		ChunkID:       "func-obsolete",
		Kind:          "func",
		Symbol:        "Obsolete",
		StartLine:     1,
		EndLine:       1,
		ContentHash:   strings.Repeat("0", 64),
		TokenEstimate: 1,
		IndexedAt:     time.Now().UTC(),
	}}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.SetMeta("embedding_provider", "mock:legacy-model"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var currentFound bool
	for _, chunk := range session.chunks {
		if chunk.Symbol == "Obsolete" {
			t.Fatalf("stale-provider chunk survived rebuild: %+v", chunk)
		}
		if chunk.Symbol == "Current" {
			currentFound = true
		}
	}
	if !currentFound {
		t.Fatalf("provider-rebuilt session has no Current chunk: %+v", session.chunks)
	}

	db, err = storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close index: %v", err)
		}
	}()
	requiresReindex, err := indexer.RequiresEmbeddingReindex(db, "mock", "mock-lcg")
	if err != nil {
		t.Fatal(err)
	}
	if requiresReindex {
		t.Fatal("NewSession did not persist the current embedding provider/model")
	}
}

func TestNewSessionRebuildsStaleIndexerVersion(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	path := filepath.Join(repo, "stable.go")
	if err := os.WriteFile(
		path,
		[]byte("package stable\n\nfunc Current() string { return \"current\" }\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.UpdateChunks(path, []models.Chunk{{
		FilePath:      path,
		ChunkID:       "func-obsolete",
		Kind:          "func",
		Symbol:        "Obsolete",
		StartLine:     1,
		EndLine:       1,
		ContentHash:   strings.Repeat("0", 64),
		TokenEstimate: 1,
		IndexedAt:     time.Now().UTC(),
	}}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.SetMeta("indexer_version", "legacy"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var currentFound bool
	for _, chunk := range session.chunks {
		if chunk.Symbol == "Obsolete" {
			t.Fatalf("stale chunk survived binary-version rebuild: %+v", chunk)
		}
		if chunk.Symbol == "Current" {
			currentFound = true
		}
	}
	if !currentFound {
		t.Fatalf("rebuilt session has no Current chunk: %+v", session.chunks)
	}

	db, err = storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close index: %v", err)
		}
	}()
	requiresReindex, err := indexer.RequiresReindex(db)
	if err != nil {
		t.Fatal(err)
	}
	if requiresReindex {
		t.Fatal("NewSession did not persist the current indexer version")
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
		// Singular/plural variants are the same exact identifier intent.
		{"Argument", []string{"arguments", "parse"}, true},
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

func TestSymbolQueryCoverage(t *testing.T) {
	tests := []struct {
		symbol string
		terms  []string
		want   int
	}{
		{"baseCreateRenderer.mountComponent", []string{"mount", "components", "renderer"}, 2},
		{"patchChildren", []string{"patch", "children"}, 2},
		{"currentIndexRevision", []string{"correction", "index", "revision", "pool"}, 2},
		{"mountChildren", []string{"mount", "components", "patch", "children"}, 0},
		{"patchBlockChildren", []string{"patch", "children"}, 0},
		{"Component", []string{"component", "setup"}, 0},
		{"CliRunner", []string{"runner", "testing"}, 0},
		{"test_other_command_invoke", []string{"invoke", "commands"}, 0},
	}
	for _, tc := range tests {
		if got := symbolQueryCoverage(tc.symbol, tc.terms); got != tc.want {
			t.Errorf("symbolQueryCoverage(%q, %v) = %d, want %d", tc.symbol, tc.terms, got, tc.want)
		}
	}
}

func TestFactorySymbolMatchesQuery(t *testing.T) {
	if !factorySymbolMatchesQuery("createRenderer", []string{"renderer", "mount"}) {
		t.Fatal("createRenderer should match a renderer query")
	}
	for _, symbol := range []string{"Renderer", "CreateComponentPublicInstance", "destroyRenderer"} {
		if factorySymbolMatchesQuery(symbol, []string{"renderer", "component"}) {
			t.Fatalf("%s should not be treated as the named factory", symbol)
		}
	}
}

func TestAppendStructuralContextUsesSlackForParentAndNamedMember(t *testing.T) {
	selected := []Hit{
		{Path: "src/click/testing.py", Symbol: "Result.output", StartLine: 10, ContentHash: "output"},
	}
	ranked := []Hit{
		selected[0],
		{
			Path: "src/click/testing.py", Kind: "class", Symbol: "CliRunner",
			ChunkID: "class-clirunner", StartLine: 20, ContentHash: "class", TokenEstimate: 500,
		},
		{
			Path: "src/click/testing.py", Kind: "method", Symbol: "CliRunner.invoke",
			ParentID: "class-clirunner", StartLine: 40, ContentHash: "invoke", TokenEstimate: 900,
		},
	}

	got := appendStructuralContext(selected, ranked, []string{"testing", "runner", "invoke", "output"})
	if len(got) != 3 {
		t.Fatalf("expanded hits = %+v, want primary + class + named member", got)
	}
	if got[1].Symbol != "CliRunner" || got[2].Symbol != "CliRunner.invoke" {
		t.Fatalf("unexpected expansion order: %+v", got)
	}
}

func TestAppendStructuralContextPrefersSelectedMembersParent(t *testing.T) {
	selected := []Hit{{
		Path: "src/click/testing.py", Kind: "method", Symbol: "CliRunner.invoke",
		ParentID: "class-clirunner", StartLine: 40, ContentHash: "invoke",
	}}
	ranked := []Hit{
		selected[0],
		{
			Path: "src/click/testing.py", Kind: "class", Symbol: "_FDCapture",
			ChunkID: "class-fdcapture", StartLine: 5, ContentHash: "capture", TokenEstimate: 100,
		},
		{
			Path: "src/click/testing.py", Kind: "class", Symbol: "CliRunner",
			ChunkID: "class-clirunner", StartLine: 20, ContentHash: "runner", TokenEstimate: 500,
		},
	}

	got := appendStructuralContext(selected, ranked, []string{"testing", "runner", "invoke", "capture"})
	if len(got) < 2 || got[1].Symbol != "CliRunner" {
		t.Fatalf("first expanded header = %+v, want CliRunner", got)
	}
}

func TestAppendStructuralContextAddsImplementationCompanions(t *testing.T) {
	selected := []Hit{{
		Path: "packages/runtime-core/src/renderer.ts", Kind: "type", Symbol: "Renderer",
		StartLine: 96, ContentHash: "renderer",
	}}
	ranked := []Hit{
		selected[0],
		{
			Path: "packages/runtime-core/src/renderer.ts", Kind: "nested_func",
			Symbol: "baseCreateRenderer.mountComponent", StartLine: 120, ContentHash: "mount", TokenEstimate: 400,
		},
		{
			Path: "packages/runtime-core/src/renderer.ts", Kind: "nested_func",
			Symbol: "baseCreateRenderer.patchChildren", StartLine: 220, ContentHash: "patch", TokenEstimate: 400,
		},
		{
			Path: "packages/runtime-core/src/renderer.ts", Kind: "export_func",
			Symbol: "createRenderer", StartLine: 318, ContentHash: "create", TokenEstimate: 100,
		},
		{
			Path: "packages/runtime-core/src/renderer.ts", Kind: "type",
			Symbol: "MountComponentFn", StartLine: 80, ContentHash: "type", TokenEstimate: 10,
		},
	}

	got := appendStructuralContext(
		selected,
		ranked,
		[]string{"vue", "renderer", "mount", "components", "patch", "children"},
	)
	if len(got) != 4 {
		t.Fatalf("expanded hits = %+v, want primary plus three implementation companions", got)
	}
	for i, want := range []string{
		"baseCreateRenderer.mountComponent",
		"baseCreateRenderer.patchChildren",
		"createRenderer",
	} {
		if got[i+1].Symbol != want {
			t.Fatalf("companion %d = %q, want %q", i, got[i+1].Symbol, want)
		}
	}
}

func TestAppendStructuralContextAddsUnselectedFilenameAnchor(t *testing.T) {
	selected := []Hit{{
		Path: "internal/mcp/server.go", Kind: "type", Symbol: "Server",
		StartLine: 20, ContentHash: "server",
	}}
	ranked := []Hit{
		selected[0],
		{
			Path: "internal/mcp/tools.go", Kind: "func",
			Symbol: "runTaskTool", StartLine: 400, ContentHash: "task", TokenEstimate: 200,
		},
		{
			Path: "internal/mcp/tools.go", Kind: "func",
			Symbol: "toolsList", StartLine: 178, ContentHash: "list", TokenEstimate: 900,
		},
	}

	got := appendStructuralContext(selected, ranked, []string{"tools", "server", "expose", "client"})
	if len(got) != 2 || got[1].Symbol != "toolsList" {
		t.Fatalf("expanded hits = %+v, want tools.go:toolsList filename anchor", got)
	}
}

func TestAppendStructuralContextAddsCompoundFilenameHelperAndAPI(t *testing.T) {
	selected := []Hit{{
		Path: "internal/retrieval/sessionpool.go", Kind: "var", Symbol: "pool",
		StartLine: 50, ContentHash: "pool",
	}}
	ranked := []Hit{
		selected[0],
		{
			Path: "internal/retrieval/sessionpool.go", Kind: "func",
			Symbol: "sessionFor", StartLine: 70, ContentHash: "helper", TokenEstimate: 180,
		},
		{
			Path: "internal/retrieval/sessionpool.go", Kind: "func",
			Symbol: "SearchShared", StartLine: 30, ContentHash: "api", TokenEstimate: 140,
		},
		{
			Path: "internal/retrieval/sessionpool.go", Kind: "func",
			Symbol: "unrelatedInternal", StartLine: 120, ContentHash: "other", TokenEstimate: 100,
		},
	}

	got := appendStructuralContext(
		selected,
		ranked,
		[]string{"session", "pool", "invalidate", "cached"},
	)
	if len(got) != 3 {
		t.Fatalf("expanded hits = %+v, want primary + helper + exported API", got)
	}
	if got[1].Symbol != "sessionFor" || got[2].Symbol != "SearchShared" {
		t.Fatalf("compound filename expansion order = %+v", got)
	}
}

func TestFileStemMatchesContiguousQueryPhrase(t *testing.T) {
	if !fileStemMatchesQuery(
		"docs/phase_g5_cross_shape.md",
		[]string{"cross", "shape", "toy", "repo"},
	) {
		t.Fatal("compound filename should match the contiguous cross shape query phrase")
	}
	if fileStemMatchesQuery(
		"docs/phase_g5_cross_shape.md",
		[]string{"shape", "cross", "toy", "repo"},
	) {
		t.Fatal("reversed query terms should not match the compound filename phrase")
	}
}

func TestAppendStructuralContextDoesNotFallbackToGenericFlatFilename(t *testing.T) {
	selected := []Hit{{
		Path: "internal/memory/ledger.go", Kind: "func", Symbol: "appendEntry",
		StartLine: 20, ContentHash: "memory",
	}}
	ranked := []Hit{
		selected[0],
		{
			Path: "internal/models/ledger.go", Kind: "type",
			Symbol: "LedgerEntry", StartLine: 6, ContentHash: "model", TokenEstimate: 120,
		},
	}
	got := appendStructuralContext(
		selected,
		ranked,
		[]string{"session", "ledger", "timelines", "histories"},
	)
	if len(got) != 1 {
		t.Fatalf("generic ledger.go fallback should not be appended: %+v", got)
	}
}

func TestDedupeSameSymbol(t *testing.T) {
	hits := []Hit{
		// Three @t.overload-style stubs + the real implementation, same symbol.
		{Path: "decorators.py", Symbol: "command", Kind: "func", Score: 46, TokenEstimate: 20, StartLine: 138},
		{Path: "decorators.py", Symbol: "command", Kind: "func", Score: 46, TokenEstimate: 22, StartLine: 144},
		{Path: "decorators.py", Symbol: "command", Kind: "func", Score: 46, TokenEstimate: 400, StartLine: 160},
		// Distinct symbol in the same file must survive.
		{Path: "decorators.py", Symbol: "option", Kind: "func", Score: 44, TokenEstimate: 300, StartLine: 220},
		// Same symbol name in a different file is a different declaration.
		{Path: "core.py", Symbol: "command", Kind: "method", Score: 40, TokenEstimate: 50, StartLine: 10},
		// Unnamed file-kind chunks are exempt from deduping.
		{Path: "README.md", Symbol: "", Kind: "file", Score: 30, TokenEstimate: 100, StartLine: 1},
		{Path: "README.md", Symbol: "", Kind: "file", Score: 29, TokenEstimate: 100, StartLine: 1},
	}
	out := dedupeSameSymbol(hits)
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5 (3 stubs collapsed to 1): %+v", len(out), out)
	}
	// The kept decorators.py/command hit is the implementation body, at the
	// first occurrence's position and score.
	if out[0].TokenEstimate != 400 || out[0].StartLine != 160 {
		t.Errorf("kept chunk should be the largest body: %+v", out[0])
	}
	if out[0].Score != 46 {
		t.Errorf("kept chunk keeps first occurrence's score, got %v", out[0].Score)
	}
	if out[1].Symbol != "option" {
		t.Errorf("distinct symbol squeezed out: %+v", out[1])
	}
}
