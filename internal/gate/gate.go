// Package gate evaluates the pivot-readiness criteria defined in
// docs/PIVOT_GATE.md against artefacts the local product already produces:
//
//   - G1 — quality.jsonl, the human ratings appended by `task --rate`.
//   - G2 — audit/bundles/*.json, the bundle snapshots persisted by `task`
//     and `pack --save-bundle`.
//   - G3 — audit/facts/*.json, hand-written fixtures of (question,
//     expects_facts). The CLI runs each fixture through the current
//     ranker/packager and counts which facts the bundle recovered.
//   - G4 — drift over historical bundles. Skipped in v1 (manual via
//     `audit replay --bundle X --response Y`).
//   - G5 — cross-shape sanity. Manual; this package only operates on
//     the current repo.
//
// The package is pure: load data, score it, return a Report. No process
// invocation, no os.Exit. The CLI wraps it in cmd/neurofs gate.go and
// is the only place that runs queries against the live index.
//
// Design intent:
//
//   - Read-only. Nothing in this package writes to disk.
//   - Honest verdicts. SKIP is not failure; it's "not enough data". Only
//     measurable badness produces FAIL.
//   - Calibrated thresholds. After RepExcerpt landed, a small bundle can
//     be the better bundle — G2 therefore does NOT fail on low budget
//     utilisation. It fails only on overshoot.
package gate

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/quality"
)

// Verdict is the four-state outcome of a single criterion or the overall
// gate. Each level has a precise meaning; the CLI uses them to decide
// process exit code (FAIL → exit 1, anything else → exit 0).
type Verdict string

const (
	Pass Verdict = "PASS" // measured and within thresholds
	Warn Verdict = "WARN" // measured, within hard thresholds, but with a soft signal worth attention
	Fail Verdict = "FAIL" // measured and outside thresholds
	Skip Verdict = "SKIP" // not enough data to evaluate
)

// Criterion is the result of one gate criterion. Numbers carries the
// raw measurements so the CLI can render a compact table without
// re-parsing Detail.
type Criterion struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	Verdict Verdict            `json:"verdict"`
	Detail  string             `json:"detail"`
	Numbers map[string]float64 `json:"numbers,omitempty"`
}

// Report bundles the per-criterion verdicts and the overall verdict. It
// is JSON-serialisable so callers can pipe `neurofs gate --json` into
// other tooling without re-implementing the schema here.
//
// G3Details, when populated by the CLI, carries the per-fixture
// FactResults so the human render can show which fixture is dragging
// the aggregate down (and which facts went missing). The aggregate
// Criterion alone is too coarse for diagnosis: "mean recall 67%" tells
// you something is wrong but not what to fix. JSON consumers always
// see the full slice; the human render filters to imperfect/errored
// fixtures only.
type Report struct {
	Criteria  []Criterion  `json:"criteria"`
	Overall   Verdict      `json:"overall"`
	G3Details []FactResult `json:"g3_details,omitempty"`
}

// G1Thresholds parameterises the real-use signal. Defaults: 10 rated
// entries, 80% yes-rate. Both can be tuned for very early or very mature
// projects without code changes.
type G1Thresholds struct {
	MinSamples int
	MinYesRate float64
}

// DefaultG1Thresholds returns the documented defaults.
func DefaultG1Thresholds() G1Thresholds {
	return G1Thresholds{MinSamples: 10, MinYesRate: 0.8}
}

// EvaluateG1 inspects the rated entries and returns the criterion. SKIP
// when there is not enough signal yet (no entries, or fewer than the
// minimum sample size). FAIL only when the yes-rate is below the floor
// AND the sample size is sufficient.
func EvaluateG1(entries []quality.Entry, th G1Thresholds) Criterion {
	yes, no := 0, 0
	for _, e := range entries {
		switch e.Rating {
		case quality.RatingYes:
			yes++
		case quality.RatingNo:
			no++
		}
	}
	n := yes + no
	c := Criterion{ID: "G1", Name: "Real-use signal"}
	if n == 0 {
		c.Verdict = Skip
		c.Detail = "no rated entries yet — run `neurofs task --rate` to produce them"
		return c
	}
	rate := float64(yes) / float64(n)
	c.Numbers = map[string]float64{
		"samples":     float64(n),
		"yes":         float64(yes),
		"no":          float64(no),
		"yes_rate":    rate,
		"min_samples": float64(th.MinSamples),
		"min_rate":    th.MinYesRate,
	}
	if n < th.MinSamples {
		c.Verdict = Skip
		c.Detail = fmt.Sprintf("only %d ratings, need %d to evaluate (current yes-rate %.0f%%)",
			n, th.MinSamples, rate*100)
		return c
	}
	if rate < th.MinYesRate {
		c.Verdict = Fail
		c.Detail = fmt.Sprintf("yes-rate %.0f%% over %d ratings is below %.0f%% threshold",
			rate*100, n, th.MinYesRate*100)
		return c
	}
	c.Verdict = Pass
	c.Detail = fmt.Sprintf("yes-rate %.0f%% over %d ratings (yes=%d no=%d, threshold %.0f%%)",
		rate*100, n, yes, no, th.MinYesRate*100)
	return c
}

// BundleSnapshot is the minimal slice of a saved Bundle that G2 needs.
// LoadBundleSnapshots produces these; tests can fabricate them directly.
type BundleSnapshot struct {
	Path   string
	Used   int
	Budget int
}

// Util returns Used/Budget, or 0 when Budget is zero (badly-shaped bundle).
func (s BundleSnapshot) Util() float64 {
	if s.Budget <= 0 {
		return 0
	}
	return float64(s.Used) / float64(s.Budget)
}

// G2Result wraps the criterion plus a soft signal (LowUtilisation) that
// the CLI uses to decide whether to downgrade G2 to WARN when G3 also
// fails. We keep them together so the post-processing step in PostprocessG2
// has a single typed input.
type G2Result struct {
	Crit           Criterion
	LowUtilisation bool
}

// LowUtilisationThreshold is the median-utilisation cut-off below which
// G2 is considered "soft-low" — under that line a low yield is a hint
// (handled in PostprocessG2). Above it, low yield is acceptable: post-
// excerpt the packager often does its job with less budget.
const LowUtilisationThreshold = 0.5

// EvaluateG2 reports budget discipline. The only hard failure is OVERSHOOT
// (used > budget). Low utilisation, on its own, is intentionally fine —
// after RepExcerpt landed, a smaller bundle is often a better bundle.
//
// Median and p95 utilisation are surfaced as numbers so the operator can
// see the distribution; the CLI can decide to render them in a table.
func EvaluateG2(snapshots []BundleSnapshot) G2Result {
	c := Criterion{ID: "G2", Name: "Budget discipline"}
	if len(snapshots) == 0 {
		c.Verdict = Skip
		c.Detail = "no bundles persisted yet — run `task` or `pack --save-bundle` to produce them"
		return G2Result{Crit: c}
	}
	overshoots := []BundleSnapshot{}
	utils := make([]float64, 0, len(snapshots))
	for _, s := range snapshots {
		if s.Budget > 0 && s.Used > s.Budget {
			overshoots = append(overshoots, s)
		}
		if s.Budget > 0 {
			utils = append(utils, s.Util())
		}
	}
	sort.Float64s(utils)
	median := percentile(utils, 0.5)
	p95 := percentile(utils, 0.95)
	c.Numbers = map[string]float64{
		"bundles":     float64(len(snapshots)),
		"overshoots":  float64(len(overshoots)),
		"median_util": median,
		"p95_util":    p95,
	}
	if len(overshoots) > 0 {
		c.Verdict = Fail
		// Name the first offender so the operator can find it without
		// re-reading the directory; multiple offenders just count.
		c.Detail = fmt.Sprintf("%d of %d bundles exceed their budget — first offender: %s",
			len(overshoots), len(snapshots), filepath.Base(overshoots[0].Path))
		return G2Result{Crit: c}
	}
	c.Verdict = Pass
	c.Detail = fmt.Sprintf("%d bundles, no overshoot; median utilisation %.0f%%, p95 %.0f%%",
		len(snapshots), median*100, p95*100)
	return G2Result{Crit: c, LowUtilisation: median < LowUtilisationThreshold}
}

// Fixture is one G3 input: a question and the facts the packed bundle
// should contain. Facts are matched as case-insensitive substrings via
// audit.ScoreFacts — the same scorer used by `audit replay --facts-file`.
type Fixture struct {
	Question     string   `json:"question"`
	ExpectsFacts []string `json:"expects_facts"`
	// SourcePath is filled by LoadFixtures so the report can name the
	// failing fixture without forcing the CLI to re-walk the directory.
	SourcePath string `json:"-"`
}

// FactResult is the per-fixture outcome the CLI feeds back into EvaluateG3.
// The CLI is responsible for invoking the bundle pipeline (taskflow.Run)
// for each fixture; this package only scores the result.
type FactResult struct {
	Fixture Fixture  `json:"fixture"`
	Recall  float64  `json:"recall"`
	Hits    []string `json:"hits"`
	Misses  []string `json:"misses"`
	// Error is set when fixture execution itself failed (no index, etc.).
	// A fixture with an error counts as recall 0 and is named in the detail.
	Error string `json:"error,omitempty"`
}

// G3Thresholds parameterises fact recovery. Default: 80% mean recall.
type G3Thresholds struct {
	MinMeanRecall float64
}

// DefaultG3Thresholds returns the documented defaults.
func DefaultG3Thresholds() G3Thresholds {
	return G3Thresholds{MinMeanRecall: 0.8}
}

// EvaluateG3 averages per-fixture recall and decides the verdict. Empty
// input yields SKIP. Otherwise FAIL when mean recall is below the floor.
func EvaluateG3(results []FactResult, th G3Thresholds) Criterion {
	c := Criterion{ID: "G3", Name: "Fact recovery"}
	if len(results) == 0 {
		c.Verdict = Skip
		c.Detail = "no fixtures available — add files under audit/facts/*.json"
		return c
	}
	sum := 0.0
	perfect := 0
	failed := 0
	worst := -1
	worstRecall := 1.1
	for i, r := range results {
		sum += r.Recall
		if r.Recall >= 0.999 {
			perfect++
		}
		if r.Error != "" {
			failed++
		}
		if r.Recall < worstRecall {
			worstRecall = r.Recall
			worst = i
		}
	}
	mean := sum / float64(len(results))
	c.Numbers = map[string]float64{
		"fixtures":     float64(len(results)),
		"mean_recall":  mean,
		"perfect":      float64(perfect),
		"failed":       float64(failed),
		"worst_recall": worstRecall,
		"min_recall":   th.MinMeanRecall,
	}
	verdict := Pass
	if mean < th.MinMeanRecall {
		verdict = Fail
	}
	c.Verdict = verdict
	c.Detail = fmt.Sprintf("mean recall %.0f%% over %d fixtures (threshold %.0f%%); %d perfect",
		mean*100, len(results), th.MinMeanRecall*100, perfect)
	if verdict == Fail && worst >= 0 {
		c.Detail += fmt.Sprintf("; worst: %q at %.0f%%",
			truncate(results[worst].Fixture.Question, 50), worstRecall*100)
	}
	return c
}

// PostprocessG2 downgrades G2 from PASS to WARN when median utilisation
// is low AND G3 failed. Rationale: low utilisation alone is fine (an
// excerpt-shaped bundle is naturally smaller). Low utilisation paired
// with poor fact recall is a signal that the packager is leaving useful
// context on the table — that is the case worth flagging.
func PostprocessG2(g2 G2Result, g3 Criterion) Criterion {
	c := g2.Crit
	if c.Verdict != Pass {
		return c // FAIL stays FAIL, SKIP stays SKIP
	}
	if !g2.LowUtilisation {
		return c
	}
	if g3.Verdict != Fail {
		return c
	}
	c.Verdict = Warn
	c.Detail += " — WARN: low median utilisation correlated with poor fact recovery (G3 FAIL); consider widening selection or raising budgets"
	return c
}

// Aggregate decides the overall verdict from a list of criteria.
//
//   - Any FAIL → overall FAIL.
//   - Any WARN → overall WARN.
//   - All SKIP → overall SKIP.
//   - At least one PASS, no FAIL/WARN → overall PASS (passing what you measured).
//
// The "all SKIP" case avoids a fresh repo getting a misleading PASS for
// having nothing to evaluate against.
func Aggregate(crits []Criterion) Verdict {
	hasFail, hasWarn, passes := false, false, 0
	for _, c := range crits {
		switch c.Verdict {
		case Fail:
			hasFail = true
		case Warn:
			hasWarn = true
		case Pass:
			passes++
		}
	}
	switch {
	case hasFail:
		return Fail
	case hasWarn:
		return Warn
	case passes == 0:
		return Skip
	default:
		return Pass
	}
}

// renderMaxMissing caps how many missing facts we print per fixture.
// Three is enough to give the operator a concrete starting point for
// investigation; more than that crowds the terminal and the JSON path
// already has the full list for anyone who wants to see all of them.
const renderMaxMissing = 3

// Render writes the report as a human-readable table. JSON output is
// the caller's job (just json.Marshal the Report). Two columns: ID/Name
// and Verdict; one detail line per criterion; an optional per-fixture
// breakdown for G3 when any fixture is imperfect or errored; one
// Overall line at the bottom. Designed for terminals; no colours or
// unicode beyond ASCII.
func Render(w io.Writer, r Report) {
	fmt.Fprintln(w, "NeuroFS — pivot-readiness gate")
	fmt.Fprintln(w)
	for _, c := range r.Criteria {
		fmt.Fprintf(w, "  %-2s  %-22s %-4s  %s\n", c.ID, c.Name, string(c.Verdict), c.Detail)
	}
	renderG3FixtureDetail(w, r.G3Details)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Overall: %s\n", string(r.Overall))
}

// renderG3FixtureDetail prints one line per imperfect or errored
// fixture so the operator can see which fixture is dragging the
// aggregate. Perfect fixtures (recall >= 0.999) are skipped — when
// every fixture is perfect there is nothing actionable to show, and
// listing them anyway would just add noise.
//
// Format:
//
//	G3 imperfect fixtures:
//	  [ 67%] "How does the ranker score filename matches and ..." — missing: filename_match
//	  [error] "broken question" — taskflow: open index: foo
func renderG3FixtureDetail(w io.Writer, results []FactResult) {
	if len(results) == 0 {
		return
	}
	imperfect := make([]FactResult, 0, len(results))
	for _, r := range results {
		if r.Error != "" || r.Recall < 0.999 {
			imperfect = append(imperfect, r)
		}
	}
	if len(imperfect) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  G3 imperfect fixtures:")
	for _, r := range imperfect {
		q := truncate(r.Fixture.Question, 60)
		if r.Error != "" {
			fmt.Fprintf(w, "    [error] %q — %s\n", q, r.Error)
			continue
		}
		fmt.Fprintf(w, "    [%3.0f%%] %q%s\n",
			r.Recall*100, q, formatMisses(r.Misses))
	}
}

// formatMisses returns " — missing: a, b, c" or " — missing: a, b, c (+N more)",
// or "" when there is nothing to show. The cap mirrors renderMaxMissing.
func formatMisses(misses []string) string {
	if len(misses) == 0 {
		return ""
	}
	shown := misses
	extra := 0
	if len(shown) > renderMaxMissing {
		extra = len(shown) - renderMaxMissing
		shown = shown[:renderMaxMissing]
	}
	out := " — missing: " + strings.Join(shown, ", ")
	if extra > 0 {
		out += fmt.Sprintf(" (+%d more)", extra)
	}
	return out
}

// percentile returns the p-percentile of a sorted ascending slice using
// the standard nearest-rank method (idx = ceil(p * n) - 1, clamped).
// Empty slice → 0. p outside [0,1] is clamped to the endpoints. The
// method matches NIST handbook §1.3.5.6 method definition; for small N
// (tens, which is our regime) it gives stable, intuitive answers — p50
// of [0.25, 0.50, 0.80, 0.90] is the lower median 0.50, p95 of the
// same is the top value 0.90.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// ─── Loaders ────────────────────────────────────────────────────────────

// LoadQualityEntries reads the JSONL produced by `task --rate`. A missing
// file is not an error: callers want to evaluate G1 as SKIP in that case,
// not fail the whole gate. Malformed lines are skipped with no fuss; a
// rating dataset is append-only and a single corrupt line should not
// invalidate every other reading.
func LoadQualityEntries(path string) ([]quality.Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gate: read quality log: %w", err)
	}
	var out []quality.Entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e quality.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate corrupt lines; do not poison the dataset
		}
		out = append(out, e)
	}
	return out, nil
}

// LoadBundleSnapshots walks dir for *.json files and parses each as a
// Bundle. Only the stats we need for G2 are extracted. A non-existent
// directory is not an error (G2 reports SKIP).
func LoadBundleSnapshots(dir string) ([]BundleSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gate: read bundles dir: %w", err)
	}
	var out []BundleSnapshot
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable; do not abort
		}
		var b models.Bundle
		if err := json.Unmarshal(data, &b); err != nil {
			continue
		}
		out = append(out, BundleSnapshot{
			Path:   path,
			Used:   b.Stats.TokensUsed,
			Budget: b.Stats.TokensBudget,
		})
	}
	// Stable ordering for reproducible reports.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// LoadFixtures walks dir for *.json fixtures. SourcePath is populated so
// the report can name the source file when a fixture fails. A non-existent
// directory is not an error (G3 reports SKIP).
func LoadFixtures(dir string) ([]Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gate: read fixtures dir: %w", err)
	}
	var out []Fixture
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("gate: read fixture %s: %w", path, err)
		}
		var f Fixture
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("gate: parse fixture %s: %w", path, err)
		}
		if strings.TrimSpace(f.Question) == "" {
			return nil, fmt.Errorf("gate: fixture %s has empty question", path)
		}
		f.SourcePath = path
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourcePath < out[j].SourcePath })
	return out, nil
}

// ScoreBundleAgainstFacts concatenates every fragment's content and runs
// audit.ScoreFacts on the joined text. The same scorer audit replay uses,
// so G3 verdicts mean exactly what `audit replay --facts-file` would mean
// for the same fixture.
func ScoreBundleAgainstFacts(b models.Bundle, facts []string) FactResult {
	var sb strings.Builder
	for _, f := range b.Fragments {
		sb.WriteString(f.Content)
		sb.WriteByte('\n')
	}
	hits, recall := audit.ScoreFacts(sb.String(), facts)
	miss := missing(facts, hits)
	return FactResult{
		Recall: recall,
		Hits:   hits,
		Misses: miss,
	}
}

// missing returns facts not present in hits, preserving the original
// (caller-supplied) ordering. Whitespace-only entries are dropped before
// comparison so a fact list with stray blanks does not produce phantom
// misses.
func missing(facts, hits []string) []string {
	hit := make(map[string]bool, len(hits))
	for _, h := range hits {
		hit[strings.TrimSpace(h)] = true
	}
	var out []string
	for _, f := range facts {
		t := strings.TrimSpace(f)
		if t == "" {
			continue
		}
		if !hit[t] {
			out = append(out, f)
		}
	}
	return out
}
