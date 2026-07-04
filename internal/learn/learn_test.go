package learn

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/usage"
)

func TestCollectFactsPrefersIdentifiers(t *testing.T) {
	fb := usage.Feedback{
		UsefulSymbols: []string{"weightFilename", "scoreFile", "weightFilename", "ab"},
		MissingFacts:  []string{"RepExcerpt"},
		UsefulPaths:   []string{"internal/ranking/ranking.go"},
	}
	got := collectFacts(fb)
	want := []string{"weightFilename", "scoreFile", "RepExcerpt"}
	if len(got) != len(want) {
		t.Fatalf("facts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("facts = %v, want %v", got, want)
		}
	}
}

func TestCollectFactsFillsWithPathStems(t *testing.T) {
	fb := usage.Feedback{
		UsefulSymbols: []string{"Search"},
		UsefulPaths:   []string{"internal/retrieval/search.go"},
	}
	got := collectFacts(fb)
	// "search" stem dedupes against the "Search" symbol (case-insensitive),
	// so only the symbol survives.
	if len(got) != 1 || got[0] != "Search" {
		t.Fatalf("facts = %v, want [Search]", got)
	}

	fb = usage.Feedback{UsefulPaths: []string{"internal/packager/excerpt_go.go"}}
	got = collectFacts(fb)
	if len(got) != 1 || got[0] != "excerpt_go" {
		t.Fatalf("facts = %v, want [excerpt_go]", got)
	}
}

func TestPromoteWritesFixturesOnceAndKeepsLatest(t *testing.T) {
	repo := t.TempDir()
	// Facts must exist in the repo or the promotion guard drops them.
	src := "package main\n\nvar oldSymbol = 1\n\nfunc scoreFile() {}\n\nconst weightFilename = 3.0\n"
	if err := os.WriteFile(filepath.Join(repo, "ranking.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two feedbacks on the same query: the later one must win.
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:         "how does ranking work",
		Rating:        usage.RatingPartial,
		UsefulSymbols: []string{"oldSymbol"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:         "How does ranking work",
		Rating:        usage.RatingYes,
		UsefulSymbols: []string{"scoreFile", "weightFilename"},
	}); err != nil {
		t.Fatal(err)
	}
	// A feedback with nothing promotable.
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:  "vague question",
		Rating: usage.RatingYes,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Promote(repo)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(res.Created) != 1 || res.Skipped != 1 {
		t.Fatalf("result = %+v, want 1 created 1 skipped", res)
	}

	data, err := os.ReadFile(res.Created[0])
	if err != nil {
		t.Fatal(err)
	}
	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Question != "How does ranking work" || f.Source != "feedback" {
		t.Fatalf("fixture = %+v", f)
	}
	if len(f.ExpectsFacts) != 2 || f.ExpectsFacts[0] != "scoreFile" {
		t.Fatalf("facts = %v, want latest feedback's facts", f.ExpectsFacts)
	}

	// Re-promotion must not overwrite the existing fixture.
	res2, err := Promote(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Created) != 0 || res2.Existing != 1 {
		t.Fatalf("second promote = %+v, want 0 created 1 existing", res2)
	}
}

func TestPromoteRetractionSuppressesEarlierFeedback(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc realSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:         "how does realSymbol work",
		Rating:        usage.RatingYes,
		UsefulSymbols: []string{"realSymbol"},
	}); err != nil {
		t.Fatal(err)
	}
	// Later factless entry = retraction; it must win over the promotable one.
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:   "How does realSymbol work",
		Rating:  usage.RatingYes,
		Comment: "retracted: filed against the wrong repo",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Promote(repo)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(res.Created) != 0 || res.Skipped != 1 {
		t.Fatalf("result = %+v, want 0 created 1 skipped (retraction wins)", res)
	}
}

func TestPromoteDropsFactsAbsentFromRepo(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed — the promotion guard passes facts through without it")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc realSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A feedback mixing one real identifier with one from another codebase
	// (e.g. filed against the wrong repo's ledger).
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:         "how does realSymbol work",
		Rating:        usage.RatingPartial,
		UsefulSymbols: []string{"realSymbol", "mountComponent"},
	}); err != nil {
		t.Fatal(err)
	}
	// A feedback whose facts are ALL foreign must not mint a fixture at all.
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query:        "how does the Vue renderer patch children",
		Rating:       usage.RatingNo,
		MissingFacts: []string{"patchChildren", "createRenderer"},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Promote(repo)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(res.Created) != 1 || res.Skipped != 1 {
		t.Fatalf("result = %+v, want 1 created (real fact kept) and 1 skipped (all-foreign)", res)
	}
	data, err := os.ReadFile(res.Created[0])
	if err != nil {
		t.Fatal(err)
	}
	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.ExpectsFacts) != 1 || f.ExpectsFacts[0] != "realSymbol" {
		t.Fatalf("facts = %v, want only the repo-present identifier", f.ExpectsFacts)
	}
}

func TestCandidateValuesProbesZeroWeights(t *testing.T) {
	mults := []float64{0.5, 1.5}
	got := candidateValues(8.0, mults)
	if len(got) != 2 || got[0] != 4.0 || got[1] != 12.0 {
		t.Fatalf("nonzero base = %v, want [4 12]", got)
	}
	// A zero weight must get fixed probes — 0×m is 0 forever otherwise.
	got = candidateValues(0, mults)
	if len(got) == 0 {
		t.Fatal("zero base returned no candidates")
	}
	for _, v := range got {
		if v <= 0 {
			t.Fatalf("zero-base probes = %v, want all positive", got)
		}
	}
}

func TestBetterThanRecallFirstThenTokens(t *testing.T) {
	hiRecall := EvalSummary{MeanRecall: 0.9, MeanTokens: 5000}
	loRecall := EvalSummary{MeanRecall: 0.8, MeanTokens: 100}
	if !betterThan(hiRecall, loRecall) {
		t.Error("higher recall must win regardless of tokens")
	}
	cheap := EvalSummary{MeanRecall: 0.9, MeanTokens: 1000}
	if !betterThan(cheap, hiRecall) {
		t.Error("at equal recall fewer tokens must win")
	}
	if betterThan(hiRecall, hiRecall) {
		t.Error("equal summaries are not an improvement")
	}
}

func newIndexedRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")

	repo := t.TempDir()
	write := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(repo, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("parser.go", "package main\n\nfunc ParseFunction(input string) string {\n\treturn input\n}\n")
	write("ranking.go", "package main\n\nfunc RankResults(items []string) []string {\n\treturn items\n}\n")

	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestTuneEndToEndImprovesOrKeepsBaseline(t *testing.T) {
	repo := newIndexedRepo(t)

	fixture := Fixture{
		Question:     "how does ParseFunction parse input",
		ExpectsFacts: []string{"ParseFunction"},
	}
	if err := os.MkdirAll(FactsDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(fixture)
	if err := os.WriteFile(filepath.Join(FactsDir(repo), "parse.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Tune(context.Background(), repo, TuneOptions{
		Passes:      1,
		Multipliers: []float64{0.5, 1.5},
		Apply:       true,
	})
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if res.Fixtures != 1 {
		t.Fatalf("fixtures = %d, want 1", res.Fixtures)
	}
	if betterThan(res.Baseline, res.Tuned) {
		t.Fatalf("tuned (%+v) must never be worse than baseline (%+v)",
			res.Tuned, res.Baseline)
	}
	if res.Warning == "" {
		t.Error("small fixture set must carry an overfit warning")
	}
	if !res.Applied {
		t.Error("apply requested but not applied")
	}
	if _, err := os.Stat(retrieval.WeightsPath(repo)); err != nil {
		t.Errorf("weights.json not written: %v", err)
	}

	// The applied weights must be what a fresh Search would load.
	w, existed, err := retrieval.LoadWeights(repo)
	if err != nil || !existed {
		t.Fatalf("load applied weights: existed=%v err=%v", existed, err)
	}
	if w != res.Weights {
		t.Errorf("persisted weights %+v differ from result %+v", w, res.Weights)
	}
}

func TestTuneMultiCorpusMacroAverages(t *testing.T) {
	repoA := newIndexedRepo(t)
	repoB := newIndexedRepo(t)

	writeFixture := func(repo, name string, f Fixture) {
		t.Helper()
		if err := os.MkdirAll(FactsDir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(f)
		if err := os.WriteFile(filepath.Join(FactsDir(repo), name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(repoA, "parse.json", Fixture{
		Question:     "how does ParseFunction parse input",
		ExpectsFacts: []string{"ParseFunction"},
	})
	writeFixture(repoB, "rank.json", Fixture{
		Question:     "how does RankResults rank items",
		ExpectsFacts: []string{"RankResults"},
	})

	res, err := Tune(context.Background(), repoA, TuneOptions{
		Passes:       1,
		Multipliers:  []float64{0.5, 1.5},
		ExtraCorpora: []Corpus{{Repo: repoB}},
	})
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if res.Fixtures != 2 {
		t.Fatalf("fixtures = %d, want 2 across corpora", res.Fixtures)
	}
	if len(res.Baseline.PerCorpus) != 2 || len(res.Tuned.PerCorpus) != 2 {
		t.Fatalf("per-corpus breakdown missing: baseline=%d tuned=%d",
			len(res.Baseline.PerCorpus), len(res.Tuned.PerCorpus))
	}
	// Macro-average: mean of the two per-corpus means.
	wantRecall := (res.Tuned.PerCorpus[0].MeanRecall + res.Tuned.PerCorpus[1].MeanRecall) / 2
	if diff := res.Tuned.MeanRecall - wantRecall; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("macro recall = %v, want %v", res.Tuned.MeanRecall, wantRecall)
	}
	if betterThan(res.Baseline, res.Tuned) {
		t.Fatalf("tuned must never be worse than baseline")
	}
	for _, fs := range res.Tuned.PerFixture {
		if fs.Corpus == "" {
			t.Error("per-fixture corpus attribution missing")
		}
	}
}

func TestLoadCorporaRejectsEmptyFixtureSet(t *testing.T) {
	repoA := newIndexedRepo(t)
	if err := os.MkdirAll(FactsDir(repoA), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(Fixture{Question: "q", ExpectsFacts: []string{"ParseFunction"}})
	if err := os.WriteFile(filepath.Join(FactsDir(repoA), "q.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	emptyRepo := t.TempDir()
	_, _, err := loadCorpora(repoA, "", []Corpus{{Repo: emptyRepo}})
	if err == nil {
		t.Fatal("empty corpus must fail loudly, not silently drop from the macro-average")
	}
}

func TestStatusCountsSignal(t *testing.T) {
	repo := newIndexedRepo(t)
	if _, err := usage.Append(repo, usage.Entry{Query: "q1", Source: "mcp", Tool: "neurofs_search"}); err != nil {
		t.Fatal(err)
	}
	if err := usage.AppendFeedback(repo, usage.Feedback{
		Query: "q1", Rating: usage.RatingYes, UsefulSymbols: []string{"ParseFunction"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Promote(repo); err != nil {
		t.Fatal(err)
	}

	st, err := Status(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.UsageCount != 1 || st.FeedbackCount != 1 || st.LearnedFixtures != 1 || st.HandFixtures != 0 {
		t.Fatalf("status = %+v", st)
	}
	if st.WeightsCustom {
		t.Error("no weights.json yet — WeightsCustom must be false")
	}
}
