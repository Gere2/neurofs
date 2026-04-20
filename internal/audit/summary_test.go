package audit

import (
	"math"
	"testing"
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
