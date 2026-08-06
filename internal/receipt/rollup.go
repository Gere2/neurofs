package receipt

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// MetricTotal aggregates one metric across runs.
//
// Known and unknown are reported side by side and never merged: a run whose
// cost the provider did not report contributes to UnknownRuns, never a zero to
// the total. A reader can then judge how much of the picture the total covers
// instead of being handed a confident-looking understatement.
type MetricTotal struct {
	Metric string
	Unit   string
	// Total is an exact decimal string over the runs with a known quantity.
	Total string
	// KnownRuns and UnknownRuns partition the runs that reported this metric.
	KnownRuns   int
	UnknownRuns int

	total *big.Rat
	scale int
}

// Complete reports whether every run reporting this metric had a known
// quantity. A total that is not complete is a lower bound, not a measurement.
func (m MetricTotal) Complete() bool { return m.UnknownRuns == 0 }

// Rollup is the scoreboard over a folded ledger.
type Rollup struct {
	Runs             int
	VerifiedRuns     int
	ByClassification map[Classification]int
	ByHumanOutcome   map[HumanOutcome]int
	// Metrics is sorted by metric then unit, so output is stable.
	Metrics []MetricTotal
}

// Summarize aggregates folded run views. It performs exact decimal arithmetic:
// quantities are parsed as rationals and rendered back at the widest scale any
// contributing figure used, so no sum is ever routed through a float.
func Summarize(views []RunView) (Rollup, error) {
	out := Rollup{
		ByClassification: make(map[Classification]int),
		ByHumanOutcome:   make(map[HumanOutcome]int),
	}
	totals := make(map[string]*MetricTotal)

	for _, v := range views {
		out.Runs++
		out.ByClassification[v.Classification]++
		out.ByHumanOutcome[v.HumanOutcome]++
		if v.Verified() {
			out.VerifiedRuns++
		}

		for _, u := range v.Usage {
			key := u.Metric + "\x00" + u.Unit
			t, ok := totals[key]
			if !ok {
				t = &MetricTotal{Metric: u.Metric, Unit: u.Unit, total: new(big.Rat)}
				totals[key] = t
			}
			if u.Provenance == ProvenanceUnknown || u.Quantity == "" {
				t.UnknownRuns++
				continue
			}
			q, ok := new(big.Rat).SetString(u.Quantity)
			if !ok {
				return Rollup{}, fmt.Errorf(
					"receipt: summarize: run %s: metric %q has unparseable quantity %q",
					v.RunID, u.Metric, u.Quantity)
			}
			t.total.Add(t.total, q)
			if s := decimalScale(u.Quantity); s > t.scale {
				t.scale = s
			}
			t.KnownRuns++
		}
	}

	for _, t := range totals {
		t.Total = t.total.FloatString(t.scale)
		out.Metrics = append(out.Metrics, *t)
	}
	sort.Slice(out.Metrics, func(i, j int) bool {
		if out.Metrics[i].Metric != out.Metrics[j].Metric {
			return out.Metrics[i].Metric < out.Metrics[j].Metric
		}
		return out.Metrics[i].Unit < out.Metrics[j].Unit
	})
	return out, nil
}

// PerVerifiedResult is the headline number: a metric total divided by the
// number of verified results, rendered at the requested scale.
//
// It refuses rather than mislead. With no verified result there is nothing to
// divide by, and when any contributing figure is unknown the quotient would
// understate the true cost by an unknown amount — the caller is told which,
// and can still read Total and UnknownRuns to decide what the partial picture
// is worth.
func (r Rollup) PerVerifiedResult(metric, unit string, scale int) (string, error) {
	if scale < 0 {
		return "", fmt.Errorf("receipt: per-verified-result: negative scale %d", scale)
	}
	var found *MetricTotal
	for i := range r.Metrics {
		if r.Metrics[i].Metric == metric && r.Metrics[i].Unit == unit {
			found = &r.Metrics[i]
			break
		}
	}
	if found == nil {
		return "", fmt.Errorf("receipt: per-verified-result: no metric %q in %q", metric, unit)
	}
	if r.VerifiedRuns == 0 {
		return "", fmt.Errorf(
			"receipt: per-verified-result: no verified result to divide by (%d runs)", r.Runs)
	}
	if !found.Complete() {
		return "", fmt.Errorf(
			"receipt: per-verified-result: %s is unknown for %d of %d reporting runs; "+
				"the quotient would understate it",
			metric, found.UnknownRuns, found.KnownRuns+found.UnknownRuns)
	}
	quotient := new(big.Rat).Quo(found.total, new(big.Rat).SetInt64(int64(r.VerifiedRuns)))
	return quotient.FloatString(scale), nil
}

// decimalScale counts the digits after the decimal point.
func decimalScale(q string) int {
	if i := strings.IndexByte(q, '.'); i >= 0 {
		return len(q) - i - 1
	}
	return 0
}
