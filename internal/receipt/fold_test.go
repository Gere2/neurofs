package receipt

import (
	"strings"
	"testing"
	"time"
)

// receiptFor builds a sealed receipt for a distinct run, so a ledger can hold
// several without tripping the one-receipt-per-run rule.
func receiptFor(t *testing.T, n int, class Classification, usage []Usage) Record {
	t.Helper()
	r := validReceipt(t)
	r.ReceiptID = "rcpt-" + string(rune('a'+n))
	r.RunID = "run-" + string(rune('a'+n))
	r.Usage = usage
	if class != ClassificationCleanPass {
		r.Verification.Classification = class
	}
	mustSeal(t, &r)
	return r
}

func amendmentFor(t *testing.T, id, corrects string, at *time.Time) Record {
	t.Helper()
	r := Record{
		SchemaVersion:     SchemaVersion,
		RecordKind:        KindRunAmendment,
		ReceiptID:         id,
		CorrectsReceiptID: corrects,
		CreatedAt:         at,
	}
	return r
}

func TestFoldAppliesAmendments(t *testing.T) {
	rec := receiptFor(t, 0, ClassificationCleanPass, nil)

	first := amendmentFor(t, "amd-1", rec.ReceiptID, ts(12, 0, 0))
	first.HumanOutcome = HumanAccepted
	first.Note = "reviewed, looks right"
	mustSeal(t, &first)

	second := amendmentFor(t, "amd-2", rec.ReceiptID, ts(18, 0, 0))
	second.HumanOutcome = HumanReverted
	second.Classification = ClassificationFailed
	second.Note = "reverted next morning, broke staging"
	mustSeal(t, &second)

	views, err := Fold([]Record{rec, first, second})
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views", len(views))
	}
	v := views[0]

	if v.Amendments != 2 {
		t.Fatalf("folded %d amendments", v.Amendments)
	}
	if v.HumanOutcome != HumanReverted {
		t.Fatalf("latest outcome should win: got %q", v.HumanOutcome)
	}
	if v.Classification != ClassificationFailed {
		t.Fatalf("latest classification should win: got %q", v.Classification)
	}
	if len(v.Notes) != 2 || !strings.Contains(v.Notes[1], "staging") {
		t.Fatalf("notes not collected in order: %v", v.Notes)
	}
	// The receipt's own verification stays untouched: the fold is a view, not
	// a rewrite of history.
	if v.Verification.Classification != ClassificationCleanPass {
		t.Fatalf("fold mutated the underlying verification: %q", v.Verification.Classification)
	}
	if v.Verified() {
		t.Fatal("a reverted run must not count as verified")
	}
}

// TestFoldOrdersByCreatedAt: ledger order is not chronological order. A
// correction filed later must win even if it was appended earlier.
func TestFoldOrdersByCreatedAt(t *testing.T) {
	rec := receiptFor(t, 0, ClassificationCleanPass, nil)

	late := amendmentFor(t, "amd-late", rec.ReceiptID, ts(20, 0, 0))
	late.HumanOutcome = HumanAccepted
	mustSeal(t, &late)

	early := amendmentFor(t, "amd-early", rec.ReceiptID, ts(9, 0, 0))
	early.HumanOutcome = HumanRejected
	mustSeal(t, &early)

	// Appended late-first; chronology must still decide.
	views, err := Fold([]Record{rec, late, early})
	if err != nil {
		t.Fatal(err)
	}
	if views[0].HumanOutcome != HumanAccepted {
		t.Fatalf("chronology ignored: got %q", views[0].HumanOutcome)
	}
}

func TestFoldCorrectsUsageRatherThanAccumulating(t *testing.T) {
	rec := receiptFor(t, 0, ClassificationCleanPass, []Usage{
		{Metric: "premium_requests", Unit: "requests", Provenance: ProvenanceUnknown, Confidence: ConfidenceUnknown},
		{Metric: "prompt_tokens", Quantity: "1000", Unit: "tokens", Provenance: ProvenanceObserved, Confidence: ConfidenceMedium},
	})

	amd := amendmentFor(t, "amd-1", rec.ReceiptID, ts(12, 0, 0))
	amd.Usage = []Usage{
		{Metric: "premium_requests", Quantity: "1", Unit: "requests", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
		{Metric: "prompt_tokens", Quantity: "1200", Unit: "tokens", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
	}
	mustSeal(t, &amd)

	views, err := Fold([]Record{rec, amd})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Usage{}
	for _, u := range views[0].Usage {
		got[u.Metric] = u
	}
	if len(views[0].Usage) != 2 {
		t.Fatalf("corrections were appended instead of replacing: %+v", views[0].Usage)
	}
	if got["premium_requests"].Quantity != "1" ||
		got["premium_requests"].Provenance != ProvenanceProviderReported {
		t.Fatalf("unknown figure not corrected: %+v", got["premium_requests"])
	}
	// 1200 replaces 1000; it is not 2200.
	if got["prompt_tokens"].Quantity != "1200" {
		t.Fatalf("correction accumulated instead of replacing: %q", got["prompt_tokens"].Quantity)
	}
}

func TestFoldRefusesInconsistentLedger(t *testing.T) {
	rec := receiptFor(t, 0, ClassificationCleanPass, nil)
	orphan := amendmentFor(t, "amd-orphan", "rcpt-nowhere", ts(12, 0, 0))
	orphan.Note = "correcting a receipt that is not here"
	mustSeal(t, &orphan)

	if _, err := Fold([]Record{rec, orphan}); err == nil ||
		!strings.Contains(err.Error(), "not found in set") {
		t.Fatalf("want refusal on a dangling amendment, got %v", err)
	}
}

func TestVerifiedRequiresCleanPassAndNoRejection(t *testing.T) {
	cases := []struct {
		class    Classification
		outcome  HumanOutcome
		verified bool
	}{
		{ClassificationCleanPass, HumanUnreviewed, true},
		{ClassificationCleanPass, HumanAccepted, true},
		{ClassificationCleanPass, HumanRejected, false},
		{ClassificationCleanPass, HumanReverted, false},
		{ClassificationPassNeedsReview, HumanAccepted, false},
		{ClassificationFailed, HumanAccepted, false},
		{ClassificationInconclusive, HumanUnreviewed, false},
	}
	for _, tc := range cases {
		v := RunView{Classification: tc.class, HumanOutcome: tc.outcome}
		if got := v.Verified(); got != tc.verified {
			t.Errorf("class %q outcome %q: Verified()=%v, want %v",
				tc.class, tc.outcome, got, tc.verified)
		}
	}
}

func TestSummarizeKeepsUnknownOutOfTotals(t *testing.T) {
	known := receiptFor(t, 0, ClassificationCleanPass, []Usage{
		{Metric: "usd", Quantity: "0.25", Unit: "usd", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
	})
	unknown := receiptFor(t, 1, ClassificationCleanPass, []Usage{
		{Metric: "usd", Unit: "usd", Provenance: ProvenanceUnknown, Confidence: ConfidenceUnknown},
	})

	views, err := Fold([]Record{known, unknown})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Summarize(views)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Metrics) != 1 {
		t.Fatalf("got %d metrics", len(sum.Metrics))
	}
	m := sum.Metrics[0]
	if m.Total != "0.25" {
		t.Fatalf("total %q: the unknown run must not contribute a zero", m.Total)
	}
	if m.KnownRuns != 1 || m.UnknownRuns != 1 {
		t.Fatalf("known/unknown split wrong: %d/%d", m.KnownRuns, m.UnknownRuns)
	}
	if m.Complete() {
		t.Fatal("a total with an unknown contributor is not complete")
	}
	// The headline number must refuse rather than understate.
	if _, err := sum.PerVerifiedResult("usd", "usd", 4); err == nil ||
		!strings.Contains(err.Error(), "understate") {
		t.Fatalf("want refusal on an incomplete total, got %v", err)
	}
}

func TestSummarizeExactDecimalArithmetic(t *testing.T) {
	// Values chosen because float64 cannot represent them exactly: 0.1+0.2
	// is the canonical example, and 3 * 0.1 != 0.3 in binary floating point.
	a := receiptFor(t, 0, ClassificationCleanPass, []Usage{
		{Metric: "usd", Quantity: "0.1", Unit: "usd", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
	})
	b := receiptFor(t, 1, ClassificationCleanPass, []Usage{
		{Metric: "usd", Quantity: "0.2", Unit: "usd", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
	})
	c := receiptFor(t, 2, ClassificationCleanPass, []Usage{
		{Metric: "usd", Quantity: "0.30", Unit: "usd", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
	})

	views, err := Fold([]Record{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Summarize(views)
	if err != nil {
		t.Fatal(err)
	}
	// Rendered at the widest contributing scale (0.30 has two digits).
	if got := sum.Metrics[0].Total; got != "0.60" {
		t.Fatalf("total %q, want exactly \"0.60\"", got)
	}
	if sum.VerifiedRuns != 3 {
		t.Fatalf("verified runs: %d", sum.VerifiedRuns)
	}
	per, err := sum.PerVerifiedResult("usd", "usd", 2)
	if err != nil {
		t.Fatal(err)
	}
	if per != "0.20" {
		t.Fatalf("per verified result %q, want \"0.20\"", per)
	}
}

func TestPerVerifiedResultRefusals(t *testing.T) {
	failed := receiptFor(t, 0, ClassificationFailed, []Usage{
		{Metric: "usd", Quantity: "1.00", Unit: "usd", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
	})
	views, err := Fold([]Record{failed})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Summarize(views)
	if err != nil {
		t.Fatal(err)
	}

	// Spending with nothing verified is not "infinite cost", it is undefined.
	if _, err := sum.PerVerifiedResult("usd", "usd", 2); err == nil ||
		!strings.Contains(err.Error(), "no verified result") {
		t.Fatalf("want refusal with zero verified runs, got %v", err)
	}
	if _, err := sum.PerVerifiedResult("credits", "requests", 2); err == nil ||
		!strings.Contains(err.Error(), "no metric") {
		t.Fatalf("want refusal for an absent metric, got %v", err)
	}
	if _, err := sum.PerVerifiedResult("usd", "usd", -1); err == nil {
		t.Fatal("negative scale must be rejected")
	}
}

// TestSummarizeCountsClassificationsAndOutcomes pins the scoreboard's shape:
// a single blended score is exactly what this must never produce.
func TestSummarizeCountsClassificationsAndOutcomes(t *testing.T) {
	clean := receiptFor(t, 0, ClassificationCleanPass, nil)
	dirty := receiptFor(t, 1, ClassificationPassNeedsReview, nil)
	failed := receiptFor(t, 2, ClassificationFailed, nil)

	views, err := Fold([]Record{clean, dirty, failed})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Summarize(views)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Runs != 3 || sum.VerifiedRuns != 1 {
		t.Fatalf("runs=%d verified=%d", sum.Runs, sum.VerifiedRuns)
	}
	if sum.ByClassification[ClassificationCleanPass] != 1 ||
		sum.ByClassification[ClassificationPassNeedsReview] != 1 ||
		sum.ByClassification[ClassificationFailed] != 1 {
		t.Fatalf("classification counts: %+v", sum.ByClassification)
	}
	if sum.ByHumanOutcome[HumanUnreviewed] != 3 {
		t.Fatalf("outcome counts: %+v", sum.ByHumanOutcome)
	}
}
