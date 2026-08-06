package gate

import (
	"encoding/json"
	"fmt"

	"github.com/Gere2/neurofs/internal/fsutil"
)

const maxBaselineBytes = int64(16 << 20)

// LoadBaseline reads a prior `neurofs gate --json` output from disk so it
// can be diffed against the current run. Returns a zero Report and a
// helpful error when the path is missing or malformed — the caller
// surfaces this to the CI runner rather than silently skipping the diff.
func LoadBaseline(path string) (Report, error) {
	data, _, err := fsutil.ReadRegularFileBounded(path, maxBaselineBytes)
	if err != nil {
		return Report{}, fmt.Errorf("baseline: read %s: %w", path, err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return Report{}, fmt.Errorf("baseline: parse %s: %w", path, err)
	}
	return r, nil
}

// Diff returns regressions present in `current` relative to `baseline`.
// A regression is one of:
//   - verdict_downgrade: a criterion's verdict moved toward FAIL
//     (PASS → SKIP/WARN/FAIL, SKIP → WARN/FAIL, or WARN → FAIL).
//   - fixture_failed: a G3 fixture whose recall was 1.0 in the baseline
//     is now below 1.0 in current.
//   - recall_dropped: a G3 fixture whose recall dropped by more than
//     5 percentage points even when both runs are below 1.0.
//   - fixture_removed: a G3 fixture present in the baseline is absent
//     from current, which is a loss of regression coverage.
//
// New criteria or new fixtures present in current but not in baseline
// are not regressions — adding coverage cannot regress.
func Diff(current, baseline Report) []Regression {
	var out []Regression

	baseCrit := indexCriteriaByID(baseline.Criteria)
	for _, c := range current.Criteria {
		b, ok := baseCrit[c.ID]
		if !ok {
			continue
		}
		if verdictRank(c.Verdict) > verdictRank(b.Verdict) {
			out = append(out, Regression{
				Kind:   "verdict_downgrade",
				Where:  c.ID,
				Before: string(b.Verdict),
				After:  string(c.Verdict),
				Detail: fmt.Sprintf("%s: %s → %s", c.Name, b.Verdict, c.Verdict),
			})
		}
	}

	baseFix := indexFactsByQuestion(baseline.G3Details)
	currentFix := indexFactsByQuestion(current.G3Details)
	for _, f := range current.G3Details {
		b, ok := baseFix[f.Fixture.Question]
		if !ok {
			continue
		}
		where := f.Fixture.SourcePath
		if where == "" {
			where = f.Fixture.Question
		}
		switch {
		case b.Recall == 1.0 && f.Recall < 1.0:
			out = append(out, Regression{
				Kind:   "fixture_failed",
				Where:  where,
				Before: "recall=1.00",
				After:  fmt.Sprintf("recall=%.2f", f.Recall),
				Detail: fmt.Sprintf("fixture %q dropped from full recall to %.0f%%; missing: %v",
					f.Fixture.Question, f.Recall*100, f.Misses),
			})
		case b.Recall-f.Recall > 0.05:
			out = append(out, Regression{
				Kind:   "recall_dropped",
				Where:  where,
				Before: fmt.Sprintf("recall=%.2f", b.Recall),
				After:  fmt.Sprintf("recall=%.2f", f.Recall),
				Detail: fmt.Sprintf("fixture %q dropped from %.0f%% to %.0f%%",
					f.Fixture.Question, b.Recall*100, f.Recall*100),
			})
		}
	}
	for _, b := range baseline.G3Details {
		if _, ok := currentFix[b.Fixture.Question]; ok {
			continue
		}
		where := b.Fixture.SourcePath
		if where == "" {
			where = b.Fixture.Question
		}
		out = append(out, Regression{
			Kind:   "fixture_removed",
			Where:  where,
			Before: fmt.Sprintf("recall=%.2f", b.Recall),
			After:  "removed",
			Detail: fmt.Sprintf("fixture %q is absent from the current report", b.Fixture.Question),
		})
	}
	return out
}

func indexCriteriaByID(crits []Criterion) map[string]Criterion {
	m := make(map[string]Criterion, len(crits))
	for _, c := range crits {
		m[c.ID] = c
	}
	return m
}

func indexFactsByQuestion(facts []FactResult) map[string]FactResult {
	m := make(map[string]FactResult, len(facts))
	for _, f := range facts {
		m[f.Fixture.Question] = f
	}
	return m
}

// verdictRank orders verdicts for diff purposes. SKIP ranks below PASS
// because losing enough evidence to make a previously measured criterion
// unevaluable is itself a regression.
func verdictRank(v Verdict) int {
	switch v {
	case Pass:
		return 0
	case Skip:
		return 1
	case Warn:
		return 2
	case Fail:
		return 3
	}
	return -1
}
