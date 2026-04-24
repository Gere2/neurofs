package audit

import (
	"math"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestAggregateFromEmptyIsZero(t *testing.T) {
	agg := AggregateFrom(nil)
	if agg.Records != 0 || agg.GroundedRatio != 0 || agg.DriftRate != 0 {
		t.Fatalf("empty aggregate should be zero-valued, got %+v", agg)
	}
}

func TestAggregateFromAveragesGroundedAndDrift(t *testing.T) {
	records := []AuditRecord{
		{
			Model:         "claude-manual",
			GroundedRatio: 1.0,
			Drift:         DriftReport{Rate: 0.1},
		},
		{
			Model:         "claude-manual",
			GroundedRatio: 0.5,
			Drift:         DriftReport{Rate: 0.3},
		},
	}
	agg := AggregateFrom(records)
	if agg.Records != 2 {
		t.Fatalf("expected Records=2, got %d", agg.Records)
	}
	if math.Abs(agg.GroundedRatio-0.75) > 1e-9 {
		t.Fatalf("expected grounded=0.75, got %v", agg.GroundedRatio)
	}
	if math.Abs(agg.DriftRate-0.2) > 1e-9 {
		t.Fatalf("expected drift=0.2, got %v", agg.DriftRate)
	}
	if agg.Models["claude-manual"] != 2 {
		t.Fatalf("expected models counter to track claude-manual=2, got %v", agg.Models)
	}
}

func TestAggregateFromRecallExcludesRecordsWithoutFacts(t *testing.T) {
	records := []AuditRecord{
		{GroundedRatio: 1, ExpectsFacts: []string{"jwt.sign"}, AnswerRecall: 1.0},
		{GroundedRatio: 1, ExpectsFacts: []string{"decrement stock"}, AnswerRecall: 0.5},
		{GroundedRatio: 1}, // no facts -> should not drag recall down to 0
	}
	agg := AggregateFrom(records)
	if math.Abs(agg.AnswerRecall-0.75) > 1e-9 {
		t.Fatalf("recall should be averaged over records with facts only, got %v", agg.AnswerRecall)
	}
}

func TestAggregateFromNoFactsMeansZeroRecall(t *testing.T) {
	records := []AuditRecord{
		{GroundedRatio: 1},
		{GroundedRatio: 1},
	}
	agg := AggregateFrom(records)
	if agg.AnswerRecall != 0 {
		t.Fatalf("recall should be 0 when no record has facts, got %v", agg.AnswerRecall)
	}
}

// TestAggregateFromCostLedger covers the cost-ledger half of the
// aggregator: bundle_tokens_used is summed only over records carrying
// Stats, selected_context_tokens_est is derived from the persisted
// compression_ratio, selected_context_reduction_est is the difference,
// and legacy records (Stats==nil) are excluded so they don't pretend to
// be zero-cost runs.
func TestAggregateFromCostLedger(t *testing.T) {
	records := []AuditRecord{
		{
			Stats: &models.BundleStats{
				FilesIncluded:    5,
				FilesConsidered:  40,
				TokensUsed:       1000,
				TokensBudget:     3000,
				CompressionRatio: 4.0, // selected_est = 4000, reduction = 3000
			},
		},
		{
			Stats: &models.BundleStats{
				FilesIncluded:    3,
				FilesConsidered:  30,
				TokensUsed:       500,
				TokensBudget:     2000,
				CompressionRatio: 6.0, // selected_est = 3000, reduction = 2500
			},
		},
		// Legacy record — Stats==nil, must be excluded from the rollup.
		{GroundedRatio: 1},
	}
	agg := AggregateFrom(records)
	if agg.RecordsWithStats != 2 {
		t.Fatalf("expected RecordsWithStats=2, got %d", agg.RecordsWithStats)
	}
	if agg.BundleTokensUsedSum != 1500 {
		t.Errorf("BundleTokensUsedSum: want 1500, got %d", agg.BundleTokensUsedSum)
	}
	if agg.SelectedContextTokensEstSum != 7000 {
		t.Errorf("SelectedContextTokensEstSum: want 7000, got %d", agg.SelectedContextTokensEstSum)
	}
	if agg.SelectedContextReductionEstSum != 5500 {
		t.Errorf("SelectedContextReductionEstSum: want 5500, got %d", agg.SelectedContextReductionEstSum)
	}
	if agg.FilesIncludedSum != 8 || agg.FilesConsideredSum != 70 {
		t.Errorf("file sums: want 8/70, got %d/%d",
			agg.FilesIncludedSum, agg.FilesConsideredSum)
	}
	if math.Abs(agg.MeanCompressionRatio-5.0) > 1e-9 {
		t.Errorf("MeanCompressionRatio: want 5.0, got %v", agg.MeanCompressionRatio)
	}
}

// TestAggregateFromCostLedgerSkipsZeroTokenStats guards against a Stats
// pointer that is non-nil but reports zero tokens — that record should
// not push compression averages towards zero.
func TestAggregateFromCostLedgerSkipsZeroTokenStats(t *testing.T) {
	records := []AuditRecord{
		{Stats: &models.BundleStats{TokensUsed: 0, CompressionRatio: 0}},
		{Stats: &models.BundleStats{TokensUsed: 200, CompressionRatio: 3.0}},
	}
	agg := AggregateFrom(records)
	if agg.RecordsWithStats != 1 {
		t.Fatalf("zero-token Stats should be skipped, got %d with stats", agg.RecordsWithStats)
	}
	if agg.SelectedContextReductionEstSum != 400 {
		t.Errorf("SelectedContextReductionEstSum: want 400, got %d", agg.SelectedContextReductionEstSum)
	}
	if math.Abs(agg.MeanCompressionRatio-3.0) > 1e-9 {
		t.Errorf("MeanCompressionRatio: want 3.0, got %v", agg.MeanCompressionRatio)
	}
}
