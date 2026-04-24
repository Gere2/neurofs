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

	// Cost ledger — rolled up across records that carry frozen BundleStats.
	// Legacy records (Stats==nil) are excluded so they don't drag averages
	// down. RecordsWithStats tells the UI how many runs the rollup uses.
	//
	// Naming is deliberately verbose so the cost numbers cannot be
	// mis-read as "savings vs the entire repo" or "savings vs an optimal
	// baseline". They describe one specific comparison only:
	//
	//   bundle_tokens_used                   — what the bundle actually shipped
	//                                          (exact, measured by tokenbudget)
	//   selected_context_tokens_est          — estimate of what the fragments
	//                                          the packager *selected to consider*
	//                                          would have cost uncompressed
	//                                          (derived: used × compression_ratio)
	//   selected_context_reduction_est       — the difference between the two
	//                                          (derived; never negative)
	//
	// "selected" is the load-bearing word: the comparison is against the
	// fragments the ranker considered, NOT against the whole repository
	// and NOT against an oracle baseline.
	RecordsWithStats               int     `json:"records_with_stats,omitempty"`
	BundleTokensUsedSum            int     `json:"bundle_tokens_used_sum,omitempty"`
	SelectedContextTokensEstSum    int     `json:"selected_context_tokens_est_sum,omitempty"`
	SelectedContextReductionEstSum int     `json:"selected_context_reduction_est_sum,omitempty"`
	FilesIncludedSum               int     `json:"files_included_sum,omitempty"`
	FilesConsideredSum             int     `json:"files_considered_sum,omitempty"`
	MeanCompressionRatio           float64 `json:"mean_compression_ratio,omitempty"`
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
		grounded, drift float64
		recallSum       float64
		recallN         int
		models          = make(map[string]int, 4)

		// Cost ledger accumulators — only count records whose Stats field
		// is populated, so legacy records (Stats==nil) don't pretend to
		// have cost zero and pull the averages down. Names mirror the
		// Aggregate field names so the rollup math stays readable.
		costN                   int
		bundleTokensUsed        int
		selectedContextTokens   int // estimate of "uncompressed selected fragments"
		selectedContextReduced  int // bundleTokensUsed minus selectedContextTokens
		filesIncluded           int
		filesConsidered         int
		compressionRatioSum     float64
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
		if r.Stats != nil && r.Stats.TokensUsed > 0 {
			costN++
			bundleTokensUsed += r.Stats.TokensUsed
			// selected_context_tokens_est is derived: it's an *estimate*
			// of what the fragments the packager selected to consider
			// would have cost uncompressed, computed by inverting the
			// persisted compression_ratio. It is NOT the cost of pasting
			// the entire repo — only of the shortlist the ranker scored.
			selEst := int(float64(r.Stats.TokensUsed) * r.Stats.CompressionRatio)
			if selEst < r.Stats.TokensUsed {
				selEst = r.Stats.TokensUsed // never claim negative reduction
			}
			selectedContextTokens += selEst
			selectedContextReduced += selEst - r.Stats.TokensUsed
			filesIncluded += r.Stats.FilesIncluded
			filesConsidered += r.Stats.FilesConsidered
			compressionRatioSum += r.Stats.CompressionRatio
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
	if costN > 0 {
		agg.RecordsWithStats = costN
		agg.BundleTokensUsedSum = bundleTokensUsed
		agg.SelectedContextTokensEstSum = selectedContextTokens
		agg.SelectedContextReductionEstSum = selectedContextReduced
		agg.FilesIncludedSum = filesIncluded
		agg.FilesConsideredSum = filesConsidered
		agg.MeanCompressionRatio = compressionRatioSum / float64(costN)
	}
	return agg
}
