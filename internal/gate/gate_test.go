package gate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/quality"
)

func TestEvaluateG1(t *testing.T) {
	cases := []struct {
		name       string
		entries    []quality.Entry
		want       Verdict
		wantDetail string // substring
	}{
		{
			name:       "no entries → SKIP",
			entries:    nil,
			want:       Skip,
			wantDetail: "no rated entries",
		},
		{
			name: "below sample minimum → SKIP",
			entries: []quality.Entry{
				{Rating: quality.RatingYes},
				{Rating: quality.RatingYes},
				{Rating: quality.RatingNo},
			},
			want:       Skip,
			wantDetail: "need 10",
		},
		{
			name:       "below yes-rate floor → FAIL",
			entries:    mkRatings(7, 5, 0), // 7y/5n = 58%, threshold 80%
			want:       Fail,
			wantDetail: "below 80% threshold",
		},
		{
			name:       "above floor → PASS",
			entries:    mkRatings(11, 2, 1), // 11y/2n over 13 ratings, skips ignored
			want:       Pass,
			wantDetail: "yes-rate 85%",
		},
		{
			name:       "skips do not count",
			entries:    mkRatings(8, 1, 100),
			want:       Skip, // n=9 < 10
			wantDetail: "only 9 ratings",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateG1(c.entries, DefaultG1Thresholds())
			if got.Verdict != c.want {
				t.Errorf("verdict = %s, want %s; detail: %s", got.Verdict, c.want, got.Detail)
			}
			if !strings.Contains(got.Detail, c.wantDetail) {
				t.Errorf("detail = %q, want contains %q", got.Detail, c.wantDetail)
			}
		})
	}
}

func TestEvaluateG2_OvershootIsTheOnlyFailure(t *testing.T) {
	// Per the corrected spec: low utilisation alone is NOT a failure.
	// Only used > budget makes G2 fail. This test pins both directions.
	t.Run("no overshoot, low utilisation → PASS with low-util flag", func(t *testing.T) {
		snaps := []BundleSnapshot{
			{Path: "a.json", Used: 200, Budget: 4000}, // 5%
			{Path: "b.json", Used: 300, Budget: 4000}, // 7.5%
		}
		got := EvaluateG2(snaps)
		if got.Crit.Verdict != Pass {
			t.Errorf("low utilisation must not FAIL G2 on its own; got %s", got.Crit.Verdict)
		}
		if !got.LowUtilisation {
			t.Errorf("expected LowUtilisation=true so PostprocessG2 can downgrade later")
		}
	})

	t.Run("any overshoot → FAIL", func(t *testing.T) {
		snaps := []BundleSnapshot{
			{Path: "ok.json", Used: 800, Budget: 4000},
			{Path: "boom.json", Used: 4500, Budget: 4000},
		}
		got := EvaluateG2(snaps)
		if got.Crit.Verdict != Fail {
			t.Fatalf("expected FAIL on overshoot; got %s", got.Crit.Verdict)
		}
		if !strings.Contains(got.Crit.Detail, "boom.json") {
			t.Errorf("FAIL detail should name the offending bundle; got: %s", got.Crit.Detail)
		}
	})

	t.Run("median and p95 are reported", func(t *testing.T) {
		snaps := []BundleSnapshot{
			{Path: "a.json", Used: 1000, Budget: 4000}, // 25%
			{Path: "b.json", Used: 2000, Budget: 4000}, // 50%
			{Path: "c.json", Used: 3200, Budget: 4000}, // 80%
			{Path: "d.json", Used: 3600, Budget: 4000}, // 90%
		}
		got := EvaluateG2(snaps)
		if got.Crit.Numbers["median_util"] < 0.4 || got.Crit.Numbers["median_util"] > 0.6 {
			t.Errorf("median should be near 0.5, got %.3f", got.Crit.Numbers["median_util"])
		}
		if got.Crit.Numbers["p95_util"] < 0.7 {
			t.Errorf("p95 should be near upper end, got %.3f", got.Crit.Numbers["p95_util"])
		}
	})

	t.Run("no bundles → SKIP", func(t *testing.T) {
		got := EvaluateG2(nil)
		if got.Crit.Verdict != Skip {
			t.Errorf("no bundles must SKIP; got %s", got.Crit.Verdict)
		}
	})
}

func TestPostprocessG2_DowngradesOnlyWhenG3FailsAndUtilLow(t *testing.T) {
	mkPass := G2Result{Crit: Criterion{ID: "G2", Verdict: Pass, Detail: "ok"}, LowUtilisation: true}
	mkFail := G2Result{Crit: Criterion{ID: "G2", Verdict: Fail, Detail: "overshoot"}, LowUtilisation: true}

	g3Pass := Criterion{Verdict: Pass}
	g3Fail := Criterion{Verdict: Fail}
	g3Skip := Criterion{Verdict: Skip}

	cases := []struct {
		name string
		g2   G2Result
		g3   Criterion
		want Verdict
	}{
		{"low util + G3 PASS → keep PASS", mkPass, g3Pass, Pass},
		{"low util + G3 SKIP → keep PASS", mkPass, g3Skip, Pass},
		{"low util + G3 FAIL → downgrade to WARN", mkPass, g3Fail, Warn},
		{"FAIL stays FAIL regardless", mkFail, g3Fail, Fail},
		{"normal util + G3 FAIL → keep PASS", G2Result{Crit: Criterion{Verdict: Pass}, LowUtilisation: false}, g3Fail, Pass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PostprocessG2(c.g2, c.g3)
			if got.Verdict != c.want {
				t.Errorf("verdict = %s, want %s; detail: %s", got.Verdict, c.want, got.Detail)
			}
		})
	}
}

func TestEvaluateG3_FactRecallAggregation(t *testing.T) {
	t.Run("empty → SKIP", func(t *testing.T) {
		got := EvaluateG3(nil, DefaultG3Thresholds())
		if got.Verdict != Skip {
			t.Errorf("empty fixture set must SKIP; got %s", got.Verdict)
		}
	})

	t.Run("mean above threshold → PASS", func(t *testing.T) {
		results := []FactResult{
			{Recall: 1.0, Fixture: Fixture{Question: "q1"}},
			{Recall: 0.9, Fixture: Fixture{Question: "q2"}},
			{Recall: 0.8, Fixture: Fixture{Question: "q3"}},
		}
		got := EvaluateG3(results, DefaultG3Thresholds())
		if got.Verdict != Pass {
			t.Errorf("mean 0.9 should PASS at threshold 0.8; got %s (%s)", got.Verdict, got.Detail)
		}
		if got.Numbers["perfect"] != 1 {
			t.Errorf("expected perfect=1, got %.0f", got.Numbers["perfect"])
		}
	})

	t.Run("mean below threshold → FAIL with worst named", func(t *testing.T) {
		results := []FactResult{
			{Recall: 0.5, Fixture: Fixture{Question: "good question 1"}},
			{Recall: 0.0, Fixture: Fixture{Question: "the worst one of all"}},
			{Recall: 0.3, Fixture: Fixture{Question: "middling"}},
		}
		got := EvaluateG3(results, DefaultG3Thresholds())
		if got.Verdict != Fail {
			t.Fatalf("mean ~0.27 must FAIL at 0.8; got %s", got.Verdict)
		}
		if !strings.Contains(got.Detail, "worst one of all") {
			t.Errorf("FAIL detail should name the worst fixture; got: %s", got.Detail)
		}
	})
}

func TestScoreBundleAgainstFacts_FindsHitsInFragmentContent(t *testing.T) {
	// Direct check that the scorer wires audit.ScoreFacts onto the
	// concatenated fragment content. Two facts present, one not.
	b := models.Bundle{Fragments: []models.ContextFragment{
		{Content: "func weightFilename = 3.0; reason: filename_match"},
		{Content: "another fragment without anything special"},
	}}
	got := ScoreBundleAgainstFacts(b, []string{"weightFilename", "filename_match", "absent_fact"})
	if got.Recall < 0.66 || got.Recall > 0.67 {
		t.Errorf("recall = %.3f, want ~0.667 (2/3)", got.Recall)
	}
	if len(got.Hits) != 2 {
		t.Errorf("hits = %v, want 2", got.Hits)
	}
	if len(got.Misses) != 1 || got.Misses[0] != "absent_fact" {
		t.Errorf("misses = %v, want [absent_fact]", got.Misses)
	}
}

// Regression: the DevX agent found that renaming an identifier in
// production code did NOT fail the gate because the old name still
// appeared in `*_test.go` fragments. Test-file fragments must be excluded
// from the gate's scoring pool — only production coverage counts.
func TestScoreBundleAgainstFacts_IgnoresTestFileFragments(t *testing.T) {
	b := models.Bundle{Fragments: []models.ContextFragment{
		{RelPath: "internal/ranking/ranking.go", Content: "weightFilenameRenamed = 3.0"},
		{RelPath: "internal/ranking/ranking_test.go", Content: "want weightFilename"},
	}}
	got := ScoreBundleAgainstFacts(b, []string{"weightFilename"})
	if len(got.Hits) != 0 {
		t.Errorf("fact only present in _test.go must NOT hit; got hits=%v", got.Hits)
	}
	if got.Recall != 0 {
		t.Errorf("recall must be 0 when only test code mentions the old name; got %v", got.Recall)
	}
}

func TestAggregate_VerdictPriority(t *testing.T) {
	cases := []struct {
		name string
		in   []Criterion
		want Verdict
	}{
		{"all pass", []Criterion{{Verdict: Pass}, {Verdict: Pass}}, Pass},
		{"any fail wins", []Criterion{{Verdict: Pass}, {Verdict: Warn}, {Verdict: Fail}}, Fail},
		{"warn over pass", []Criterion{{Verdict: Pass}, {Verdict: Warn}}, Warn},
		{"all skip", []Criterion{{Verdict: Skip}, {Verdict: Skip}}, Skip},
		{"pass + skip → pass", []Criterion{{Verdict: Pass}, {Verdict: Skip}}, Pass},
		{"empty", nil, Skip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Aggregate(c.in); got != c.want {
				t.Errorf("Aggregate = %s, want %s", got, c.want)
			}
		})
	}
}

func TestPercentile_NearestRank(t *testing.T) {
	s := []float64{0.1, 0.2, 0.3, 0.4, 0.5}
	if got := percentile(s, 0.5); got != 0.3 {
		t.Errorf("p50 = %v, want 0.3", got)
	}
	if got := percentile(s, 0.95); got != 0.5 {
		t.Errorf("p95 = %v, want 0.5", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty p50 = %v, want 0", got)
	}
}

// ─── Loader tests ─────────────────────────────────────────────────────

func TestLoadQualityEntries_ToleratesMissingFileAndCorruptLines(t *testing.T) {
	t.Run("missing file → no error, empty result", func(t *testing.T) {
		entries, err := LoadQualityEntries(filepath.Join(t.TempDir(), "nope.jsonl"))
		if err != nil {
			t.Fatalf("missing file should not error: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected 0 entries, got %d", len(entries))
		}
	})

	t.Run("mixed valid + corrupt lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "quality.jsonl")
		good1, _ := json.Marshal(quality.Entry{Rating: quality.RatingYes, Query: "ok"})
		good2, _ := json.Marshal(quality.Entry{Rating: quality.RatingNo, Query: "also ok"})
		content := string(good1) + "\n" +
			"this is not json at all\n" +
			string(good2) + "\n" +
			"\n" + // blank tolerated
			"{\"unterminated\":\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		entries, err := LoadQualityEntries(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 2 {
			t.Errorf("expected 2 valid entries despite corruption, got %d", len(entries))
		}
	})
}

func TestLoadFixtures_RejectsEmptyQuestion(t *testing.T) {
	dir := t.TempDir()
	good := Fixture{Question: "what is x", ExpectsFacts: []string{"x"}}
	bad := Fixture{Question: "", ExpectsFacts: []string{"y"}}
	gd, _ := json.Marshal(good)
	bd, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, "good.json"), gd, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), bd, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFixtures(dir)
	if err == nil {
		t.Fatal("expected error for empty-question fixture")
	}
	if !strings.Contains(err.Error(), "empty question") {
		t.Errorf("error should mention the malformed fixture: %v", err)
	}
}

func TestLoadFixtures_MissingDirIsSkip(t *testing.T) {
	got, err := LoadFixtures(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing fixtures dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d", len(got))
	}
}

func TestLoadBundleSnapshots_ParsesStatsAndIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	good := models.Bundle{
		Stats: models.BundleStats{TokensUsed: 800, TokensBudget: 4000},
	}
	gd, _ := json.Marshal(good)
	if err := os.WriteFile(filepath.Join(dir, "ok.json"), gd, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBundleSnapshots(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot (junk ignored, .txt skipped), got %d", len(got))
	}
	if got[0].Used != 800 || got[0].Budget != 4000 {
		t.Errorf("snapshot fields wrong: %+v", got[0])
	}
}

// ─── Render: per-fixture G3 detail ────────────────────────────────────

func TestRender_PerFixtureDetailShownForImperfect(t *testing.T) {
	r := Report{
		Criteria: []Criterion{
			{ID: "G3", Name: "Fact recovery", Verdict: Fail, Detail: "mean recall 67%"},
		},
		Overall: Fail,
		G3Details: []FactResult{
			{
				Fixture: Fixture{Question: "Why does the ranker pick utils.py?"},
				Recall:  0.6666,
				Hits:    []string{"weightFilename"},
				Misses:  []string{"filename_match", "scoreFile"},
			},
			{
				Fixture: Fixture{Question: "perfect query"},
				Recall:  1.0,
				Hits:    []string{"a", "b"},
				Misses:  nil,
			},
		},
	}
	var buf bytes.Buffer
	Render(&buf, r)
	out := buf.String()

	// Imperfect fixture must appear with recall and missing facts.
	if !strings.Contains(out, "G3 imperfect fixtures:") {
		t.Errorf("missing detail header:\n%s", out)
	}
	if !strings.Contains(out, "[ 67%]") {
		t.Errorf("missing recall percentage:\n%s", out)
	}
	if !strings.Contains(out, "Why does the ranker pick utils.py?") {
		t.Errorf("missing fixture question:\n%s", out)
	}
	if !strings.Contains(out, "missing: filename_match, scoreFile") {
		t.Errorf("missing facts not listed:\n%s", out)
	}
	// Perfect fixture must NOT appear in the imperfect section.
	if strings.Contains(out, "perfect query") {
		t.Errorf("perfect fixture should be hidden from the imperfect section:\n%s", out)
	}
}

func TestRender_PerFixtureDetailCapsMissingAtThree(t *testing.T) {
	r := Report{
		Criteria: []Criterion{{ID: "G3", Verdict: Fail}},
		Overall:  Fail,
		G3Details: []FactResult{{
			Fixture: Fixture{Question: "many misses"},
			Recall:  0.2,
			Misses:  []string{"a", "b", "c", "d", "e"},
		}},
	}
	var buf bytes.Buffer
	Render(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "missing: a, b, c") {
		t.Errorf("expected first 3 misses, got:\n%s", out)
	}
	if !strings.Contains(out, "(+2 more)") {
		t.Errorf("expected overflow marker for extra misses:\n%s", out)
	}
	for _, leak := range []string{", d", ", e"} {
		if strings.Contains(out, leak) {
			t.Errorf("expected NOT to print fact %q (over the cap):\n%s", leak, out)
		}
	}
}

func TestRender_PerFixtureDetailShowsErrors(t *testing.T) {
	r := Report{
		Criteria: []Criterion{{ID: "G3", Verdict: Fail}},
		Overall:  Fail,
		G3Details: []FactResult{{
			Fixture: Fixture{Question: "broken fixture"},
			Recall:  0.0,
			Error:   "taskflow: open index: file not found",
		}},
	}
	var buf bytes.Buffer
	Render(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "[error]") {
		t.Errorf("expected [error] tag for errored fixture:\n%s", out)
	}
	if !strings.Contains(out, "broken fixture") {
		t.Errorf("expected fixture question:\n%s", out)
	}
	if !strings.Contains(out, "taskflow: open index: file not found") {
		t.Errorf("expected error message inline:\n%s", out)
	}
}

func TestRender_NoG3DetailSectionWhenAllPerfect(t *testing.T) {
	r := Report{
		Criteria: []Criterion{{ID: "G3", Verdict: Pass}},
		Overall:  Pass,
		G3Details: []FactResult{
			{Fixture: Fixture{Question: "q1"}, Recall: 1.0},
			{Fixture: Fixture{Question: "q2"}, Recall: 1.0},
		},
	}
	var buf bytes.Buffer
	Render(&buf, r)
	out := buf.String()

	if strings.Contains(out, "G3 imperfect fixtures:") {
		t.Errorf("perfect run should not print the imperfect section:\n%s", out)
	}
}

func TestRender_NoG3DetailSectionWhenSkipped(t *testing.T) {
	// G3 SKIP via --skip-fixtures or missing index → CLI does not
	// populate G3Details. Render must not emit a stray empty section.
	r := Report{
		Criteria: []Criterion{{ID: "G3", Verdict: Skip}},
		Overall:  Skip,
	}
	var buf bytes.Buffer
	Render(&buf, r)
	if strings.Contains(buf.String(), "G3 imperfect fixtures:") {
		t.Errorf("skipped G3 must not print the imperfect section")
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

func mkRatings(yes, no, skip int) []quality.Entry {
	out := make([]quality.Entry, 0, yes+no+skip)
	for i := 0; i < yes; i++ {
		out = append(out, quality.Entry{Rating: quality.RatingYes})
	}
	for i := 0; i < no; i++ {
		out = append(out, quality.Entry{Rating: quality.RatingNo})
	}
	for i := 0; i < skip; i++ {
		out = append(out, quality.Entry{Rating: quality.RatingSkip})
	}
	return out
}

func TestEvaluateG4(t *testing.T) {
	t.Parallel()

	t.Run("no records → SKIP", func(t *testing.T) {
		got := EvaluateG4(nil, DefaultG4Thresholds())
		if got.Verdict != Skip {
			t.Errorf("expected SKIP when no records, got %s", got.Verdict)
		}
	})

	t.Run("mean drift below threshold → PASS", func(t *testing.T) {
		records := []audit.AuditRecord{
			{Drift: audit.DriftReport{Rate: 0.10}},
			{Drift: audit.DriftReport{Rate: 0.05}},
		}
		got := EvaluateG4(records, DefaultG4Thresholds())
		if got.Verdict != Pass {
			t.Errorf("expected PASS when mean drift is 7.5%%, got %s", got.Verdict)
		}
	})

	t.Run("mean drift above threshold → FAIL", func(t *testing.T) {
		records := []audit.AuditRecord{
			{Drift: audit.DriftReport{Rate: 0.20}},
			{Drift: audit.DriftReport{Rate: 0.30}},
		}
		got := EvaluateG4(records, DefaultG4Thresholds())
		if got.Verdict != Fail {
			t.Errorf("expected FAIL when mean drift is 25%%, got %s", got.Verdict)
		}
	})
}

func TestEvaluateG4SamplesPoolsOrigins(t *testing.T) {
	t.Parallel()

	t.Run("no samples → SKIP", func(t *testing.T) {
		got := EvaluateG4Samples(nil, DefaultG4Thresholds())
		if got.Verdict != Skip {
			t.Errorf("expected SKIP, got %s", got.Verdict)
		}
	})

	t.Run("pooled mean and per-origin counts", func(t *testing.T) {
		samples := []DriftSample{
			{Origin: "record", Label: "q1", Rate: 0.10},
			{Origin: "pair", Label: "stem-a", Rate: 0.05},
			{Origin: "grounding", Label: "sess1", Rate: 0.15},
		}
		got := EvaluateG4Samples(samples, DefaultG4Thresholds())
		if got.Verdict != Pass {
			t.Fatalf("expected PASS at mean 10%%, got %s (%s)", got.Verdict, got.Detail)
		}
		if got.Numbers["records"] != 1 || got.Numbers["pairs"] != 1 || got.Numbers["grounding"] != 1 {
			t.Errorf("origin counts wrong: %+v", got.Numbers)
		}
		if got.Numbers["samples"] != 3 {
			t.Errorf("samples = %v, want 3", got.Numbers["samples"])
		}
	})

	t.Run("high pooled drift → FAIL names worst", func(t *testing.T) {
		samples := []DriftSample{
			{Origin: "pair", Label: "good", Rate: 0.05},
			{Origin: "grounding", Label: "hallucinated-session", Rate: 0.60},
		}
		got := EvaluateG4Samples(samples, DefaultG4Thresholds())
		if got.Verdict != Fail {
			t.Fatalf("expected FAIL at mean 32.5%%, got %s", got.Verdict)
		}
		if !strings.Contains(got.Detail, "hallucinated-session") {
			t.Errorf("detail should name the worst sample, got: %s", got.Detail)
		}
	})
}

func TestCollectPairDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bundlesDir := filepath.Join(dir, "bundles")
	responsesDir := filepath.Join(dir, "responses")
	if err := os.MkdirAll(bundlesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(responsesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bundle := models.Bundle{
		Query: "how does auth work",
		Fragments: []models.ContextFragment{{
			RelPath: "src/auth.ts",
			Content: "function verifyToken(token) { return jwtVerify(token) }",
		}},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundlesDir, "auth-run.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Grounded response: only identifiers the bundle contains.
	grounded := "The flow calls verifyToken which wraps jwtVerify from src/auth.ts."
	if err := os.WriteFile(filepath.Join(responsesDir, "auth-run.md"), []byte(grounded), 0o644); err != nil {
		t.Fatal(err)
	}
	// Orphan response with no matching bundle must be skipped, not scored.
	if err := os.WriteFile(filepath.Join(responsesDir, "orphan.md"), []byte("references phantomScorer everywhere"), 0o644); err != nil {
		t.Fatal(err)
	}

	samples, err := CollectPairDrift(bundlesDir, responsesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1 (orphan must be skipped): %+v", len(samples), samples)
	}
	if samples[0].Origin != "pair" || samples[0].Label != "auth-run" {
		t.Fatalf("sample identity wrong: %+v", samples[0])
	}
	if samples[0].Rate > 0.34 {
		t.Errorf("grounded response drift = %.2f, want low", samples[0].Rate)
	}

	// A drifting response against the same bundle must score strictly higher.
	drifting := "It calls phantomScorer and legacyRanker via ghost_module.deepMagic in lib/phantom.rs."
	if err := os.WriteFile(filepath.Join(responsesDir, "auth-run.md"), []byte(drifting), 0o644); err != nil {
		t.Fatal(err)
	}
	driftSamples, err := CollectPairDrift(bundlesDir, responsesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(driftSamples) != 1 {
		t.Fatalf("drift samples = %d, want 1", len(driftSamples))
	}
	if driftSamples[0].Rate <= samples[0].Rate {
		t.Errorf("drifting response (%.2f) should out-drift grounded one (%.2f)",
			driftSamples[0].Rate, samples[0].Rate)
	}

	t.Run("missing responses dir → nil, no error", func(t *testing.T) {
		got, err := CollectPairDrift(bundlesDir, filepath.Join(dir, "nope"))
		if err != nil || got != nil {
			t.Errorf("want (nil, nil), got (%+v, %v)", got, err)
		}
	})
}
