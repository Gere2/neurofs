package abeval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

// stubSearch returns canned hits keyed by query, so the A/B mechanics can be
// exercised without a live index.
func stubSearch(byQuery map[string][]SearchHit) SearchFn {
	return func(_ context.Context, q string, _ int) ([]SearchHit, error) {
		return byQuery[q], nil
	}
}

// writeRepo creates files under a temp dir and returns the FileRecords plus the
// dir. content is keyed by rel path.
func writeRepo(t *testing.T, content map[string]string) []models.FileRecord {
	t.Helper()
	dir := t.TempDir()
	var recs []models.FileRecord
	for rel, body := range content {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, models.FileRecord{Path: abs, RelPath: rel})
	}
	return recs
}

func TestRunIsoRecallReducesTokens(t *testing.T) {
	// storage.go is large; its WAL pragma lives deep in the body. neurofs_search
	// delivers just the 2-line excerpt; native must read the whole file to tie.
	bigBody := "package storage\n\nfunc Open() {}\n"
	for i := 0; i < 500; i++ {
		bigBody += "// filler line that costs tokens but holds no fact\n"
	}
	bigBody += "\tPRAGMA journal_mode = WAL\n"

	files := writeRepo(t, map[string]string{
		"internal/storage/storage.go": bigBody,
		"internal/other/other.go":     "package other\nfunc Noise() {}\n",
	})

	q := "how does storage open the database with WAL mode"
	search := stubSearch(map[string][]SearchHit{
		q: {
			// One tight excerpt carrying the fact; ~tens of tokens.
			{Path: "internal/storage/storage.go", Snippet: "PRAGMA journal_mode = WAL", Tokens: 8},
		},
	})

	tasks := []Task{{Question: q, ExpectsFacts: []string{"PRAGMA journal_mode = WAL"}, Source: "wal"}}
	results, summary, err := Run(context.Background(), files, tasks, search, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if !r.Scored {
		t.Fatalf("expected scored task, note=%q", r.Note)
	}
	if r.Neurofs.Recall < 0.999 {
		t.Fatalf("neurofs recall = %.2f, want 1.0", r.Neurofs.Recall)
	}
	if r.NativeIso.Recall < 0.999 {
		t.Fatalf("native recall = %.2f, want 1.0 (whole file is a superset)", r.NativeIso.Recall)
	}
	if r.NativeIso.Tokens <= r.Neurofs.Tokens {
		t.Fatalf("native (%d) should cost more than excerpt (%d)", r.NativeIso.Tokens, r.Neurofs.Tokens)
	}
	if r.TokenReduction <= 0 {
		t.Fatalf("token reduction = %.2f, want > 0", r.TokenReduction)
	}
	if summary.Verdict != "PASS" {
		t.Fatalf("verdict = %q (%s), want PASS", summary.Verdict, summary.Detail)
	}
}

func TestRunStopsAtIsoRecall(t *testing.T) {
	// Two hit files, each holding one of two facts. B recovers only fact A
	// (50% recall). Native should read ONLY the first file to tie at 50%, not
	// both — proving the iso-recall stop.
	files := writeRepo(t, map[string]string{
		"a.go": "FACT_ALPHA lives here\n" + filler(200),
		"b.go": "FACT_BETA lives here\n" + filler(200),
	})
	q := "alpha and beta"
	search := stubSearch(map[string][]SearchHit{
		q: {
			{Path: "a.go", Snippet: "FACT_ALPHA", Tokens: 5}, // only alpha in B's snippets
		},
	})
	tasks := []Task{{Question: q, ExpectsFacts: []string{"FACT_ALPHA", "FACT_BETA"}, Source: "ab"}}
	results, _, err := Run(context.Background(), files, tasks, search, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Neurofs.Recall > 0.51 {
		t.Fatalf("neurofs recall = %.2f, want 0.5", r.Neurofs.Recall)
	}
	if len(r.NativeIso.Files) != 1 || r.NativeIso.Files[0] != "a.go" {
		t.Fatalf("native should read only a.go to tie 50%%, got %v", r.NativeIso.Files)
	}
}

func TestRunSearchMissIsNotScored(t *testing.T) {
	files := writeRepo(t, map[string]string{"a.go": "nothing relevant\n"})
	q := "missing"
	search := stubSearch(map[string][]SearchHit{
		q: {{Path: "a.go", Snippet: "nothing relevant", Tokens: 4}},
	})
	tasks := []Task{{Question: q, ExpectsFacts: []string{"WIDGET_NOT_PRESENT"}, Source: "miss"}}
	results, summary, err := Run(context.Background(), files, tasks, search, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Scored {
		t.Fatalf("a search miss must not be scored")
	}
	if summary.SearchMiss != 1 {
		t.Fatalf("summary.SearchMiss = %d, want 1", summary.SearchMiss)
	}
	if summary.Verdict != "INSUFFICIENT" {
		t.Fatalf("verdict = %q, want INSUFFICIENT (nothing scorable)", summary.Verdict)
	}
}

func TestSummariseVerdictThreshold(t *testing.T) {
	mk := func(red float64) TaskResult {
		return TaskResult{Scored: true, TokenReduction: red,
			Neurofs: Arm{Tokens: 100, Recall: 1}, NativeIso: Arm{Tokens: 300, Recall: 1}}
	}
	pass := summarise([]TaskResult{mk(0.25), mk(0.5)}, Options{}.withDefaults())
	if pass.Verdict != "PASS" {
		t.Fatalf("verdict = %q, want PASS (%s)", pass.Verdict, pass.Detail)
	}
	fail := summarise([]TaskResult{mk(0.10), mk(0.05)}, Options{}.withDefaults())
	if fail.Verdict != "FAIL" {
		t.Fatalf("verdict = %q, want FAIL (%s)", fail.Verdict, fail.Detail)
	}
}

func TestSummariseOverallRecallCountsMisses(t *testing.T) {
	// 3 fact tasks: one B grounded fully (and saved tokens), two search misses.
	// Scored-subset recall is 100%, but the honest overall recall is 1/3, and a
	// 67% miss rate must downgrade the verdict to WARN, not PASS.
	hit := TaskResult{HasFacts: true, Scored: true, TokenReduction: 0.6,
		Neurofs: Arm{Tokens: 100, Recall: 1}, NativeIso: Arm{Tokens: 300, Recall: 1}}
	miss := TaskResult{HasFacts: true, Note: "neurofs_search recovered no facts (search miss)"}
	s := summarise([]TaskResult{hit, miss, miss}, Options{}.withDefaults())

	if s.FactTasks != 3 || s.SearchMiss != 2 || s.Scored != 1 {
		t.Fatalf("FactTasks/SearchMiss/Scored = %d/%d/%d, want 3/2/1", s.FactTasks, s.SearchMiss, s.Scored)
	}
	if s.MeanRecallNeurofs < 0.999 {
		t.Fatalf("scored-subset recall = %.2f, want 1.0", s.MeanRecallNeurofs)
	}
	if s.OverallRecallNeurofs < 0.33 || s.OverallRecallNeurofs > 0.34 {
		t.Fatalf("overall recall = %.3f, want ~0.333 (misses count as 0)", s.OverallRecallNeurofs)
	}
	if s.Verdict != "WARN" {
		t.Fatalf("verdict = %q, want WARN (high miss rate masks the savings) — %s", s.Verdict, s.Detail)
	}
}

func TestMedian(t *testing.T) {
	if got := median([]float64{0.1, 0.3, 0.2}); got != 0.2 {
		t.Fatalf("median odd = %v, want 0.2", got)
	}
	if got := median([]float64{0.2, 0.4}); got < 0.299 || got > 0.301 {
		t.Fatalf("median even = %v, want ~0.3", got)
	}
}

func filler(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += "filler line with no fact in it at all\n"
	}
	return s
}
