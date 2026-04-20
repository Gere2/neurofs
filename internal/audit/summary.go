package audit

// Aggregate is the rolled-up view of a set of AuditRecords. It is the shape
// consumed by `stats` to show governance trends, and by anyone building a
// dashboard later.
//
// All ratios are population averages over the records, not weighted by
// response length — we treat each question as one observation.
type Aggregate struct {
	Records       int     `json:"records"`
	GroundedRatio float64 `json:"grounded_ratio"`
	DriftRate     float64 `json:"drift_rate"`
	AnswerRecall  float64 `json:"answer_recall"`

	// Models counts how many records came from each model ID, so you can
	// see e.g. "12 claude-manual, 3 stub" at a glance.
	Models map[string]int `json:"models,omitempty"`
}

// AggregateFrom condenses a slice of records into a single Aggregate.
// Empty input returns a zero Aggregate (not an error): "no data" is a
// legitimate state that callers should render as "—", not fail on.
//
// AnswerRecall is averaged only over records that carried expected facts;
// records without facts have recall=0 by default and would otherwise drag
// the mean down unfairly.
func AggregateFrom(records []AuditRecord) Aggregate {
	if len(records) == 0 {
		return Aggregate{}
	}
	var (
		grounded, drift    float64
		recallSum          float64
		recallN            int
		models             = make(map[string]int, 4)
	)
	for _, r := range records {
		grounded += r.GroundedRatio
		drift += r.Drift.Rate
		if len(r.ExpectsFacts) > 0 {
			recallSum += r.AnswerRecall
			recallN++
		}
		if r.Model != "" {
			models[r.Model]++
		}
	}
	n := float64(len(records))
	agg := Aggregate{
		Records:       len(records),
		GroundedRatio: grounded / n,
		DriftRate:     drift / n,
		Models:        models,
	}
	if recallN > 0 {
		agg.AnswerRecall = recallSum / float64(recallN)
	}
	return agg
}
