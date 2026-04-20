package audit

import (
	"math"
	"testing"
)

func TestDiffRecordsScalarDeltas(t *testing.T) {
	a := AuditRecord{
		Question:      "q",
		Model:         "claude-manual",
		BundleHash:    "aaa",
		GroundedRatio: 0.6,
		Drift:         DriftReport{Rate: 0.3},
		ExpectsFacts:  []string{"x"},
		AnswerRecall:  0.5,
	}
	b := a
	b.GroundedRatio = 0.8
	b.Drift.Rate = 0.1
	b.AnswerRecall = 0.9

	d := DiffRecords(a, b)
	if math.Abs(d.GroundedDelta-0.2) > 1e-9 {
		t.Errorf("grounded delta: want +0.2, got %v", d.GroundedDelta)
	}
	if math.Abs(d.DriftDelta+0.2) > 1e-9 {
		t.Errorf("drift delta: want -0.2, got %v", d.DriftDelta)
	}
	if !d.RecallApplies || math.Abs(d.RecallDelta-0.4) > 1e-9 {
		t.Errorf("recall applies + delta: want true,+0.4, got %v,%v", d.RecallApplies, d.RecallDelta)
	}
	if !d.SameBundle || !d.SameQuestion || !d.SameModel {
		t.Errorf("same-flags should be true, got %+v", d)
	}
}

func TestDiffRecordsSetDeltas(t *testing.T) {
	a := AuditRecord{
		Drift: DriftReport{
			UnknownPaths:   []string{"src/missing.ts", "utils.py"},
			UnknownAPIs:    []string{"jwt.rotate"},
			UnknownSymbols: []string{"GhostService", "OldController"},
		},
	}
	b := AuditRecord{
		Drift: DriftReport{
			UnknownPaths:   []string{"src/missing.ts"},            // utils.py removed
			UnknownAPIs:    []string{"redis.flushDB"},              // swapped
			UnknownSymbols: []string{"GhostService", "NewService"}, // OldController -> NewService
		},
	}

	d := DiffRecords(a, b)

	if !equalStringSlices(d.Paths.Removed, []string{"utils.py"}) ||
		len(d.Paths.Added) != 0 {
		t.Errorf("paths diff mismatch: %+v", d.Paths)
	}
	if !equalStringSlices(d.APIs.Added, []string{"redis.flushDB"}) ||
		!equalStringSlices(d.APIs.Removed, []string{"jwt.rotate"}) {
		t.Errorf("apis diff mismatch: %+v", d.APIs)
	}
	if !equalStringSlices(d.Symbols.Added, []string{"NewService"}) ||
		!equalStringSlices(d.Symbols.Removed, []string{"OldController"}) {
		t.Errorf("symbols diff mismatch: %+v", d.Symbols)
	}
}

func TestDiffRecordsRecallSkippedWhenNeitherHasFacts(t *testing.T) {
	a := AuditRecord{GroundedRatio: 1, AnswerRecall: 0}
	b := AuditRecord{GroundedRatio: 1, AnswerRecall: 0}
	d := DiffRecords(a, b)
	if d.RecallApplies {
		t.Errorf("recall should not apply when neither record has facts")
	}
	if d.RecallDelta != 0 {
		t.Errorf("recall delta should stay 0 when skipped, got %v", d.RecallDelta)
	}
}

func TestDiffRecordsBundleMismatchSurfaces(t *testing.T) {
	a := AuditRecord{BundleHash: "aaa"}
	b := AuditRecord{BundleHash: "bbb"}
	d := DiffRecords(a, b)
	if d.SameBundle {
		t.Errorf("SameBundle should be false when hashes differ")
	}

	// Empty hashes on both sides should also not claim equality — the
	// "no hash" state is not the same as "matching hash".
	empty := DiffRecords(AuditRecord{}, AuditRecord{})
	if empty.SameBundle {
		t.Errorf("SameBundle should be false when both hashes are empty")
	}
}

func TestDiffRecordsModeSurfaces(t *testing.T) {
	// Matching modes → SameMode true, labels preserved for display.
	same := DiffRecords(
		AuditRecord{Mode: "build"},
		AuditRecord{Mode: "build"},
	)
	if !same.SameMode {
		t.Errorf("SameMode should be true when both records carry the same mode")
	}
	if same.ModeA != "build" || same.ModeB != "build" {
		t.Errorf("mode labels should propagate to Diff.Mode{A,B}")
	}

	// Different modes → SameMode false, labels still exposed.
	mixed := DiffRecords(
		AuditRecord{Mode: "strategy"},
		AuditRecord{Mode: "build"},
	)
	if mixed.SameMode {
		t.Errorf("SameMode should be false when modes differ")
	}
	if mixed.ModeA != "strategy" || mixed.ModeB != "build" {
		t.Errorf("mode labels should propagate even when different, got %q/%q", mixed.ModeA, mixed.ModeB)
	}

	// Both empty (e.g. legacy records) → SameMode false, same rationale as
	// SameBundle: "no label" is not the same as "matching label".
	empty := DiffRecords(AuditRecord{}, AuditRecord{})
	if empty.SameMode {
		t.Errorf("SameMode should be false when neither record has a mode")
	}
}

func TestDiffRecordsEmptyBucketsProduceEmptyDiff(t *testing.T) {
	d := DiffRecords(AuditRecord{}, AuditRecord{})
	if len(d.Paths.Added)+len(d.Paths.Removed) != 0 {
		t.Errorf("expected zero path diff, got %+v", d.Paths)
	}
	if len(d.APIs.Added)+len(d.APIs.Removed) != 0 {
		t.Errorf("expected zero api diff, got %+v", d.APIs)
	}
	if len(d.Symbols.Added)+len(d.Symbols.Removed) != 0 {
		t.Errorf("expected zero symbol diff, got %+v", d.Symbols)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
