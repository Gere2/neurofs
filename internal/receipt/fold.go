package receipt

import (
	"fmt"
	"sort"
	"time"
)

// RunView is the effective state of one run: its receipt with every amendment
// applied. The ledger keeps both the original and the corrections; a reader
// wants the conclusion, so folding happens here and never on disk.
//
// Amendments correct, they do not accumulate: a later figure for the same
// metric supersedes the earlier one (a billing dashboard confirming what was
// unknown at write time is a correction, not a second charge). Ordering is by
// created_at, with ledger position breaking ties so the fold is deterministic.
type RunView struct {
	ReceiptID  string
	TaskID     string
	RunID      string
	StartedAt  time.Time
	FinishedAt time.Time
	Repo       Repo
	Policy     Policy
	Attempts   []Attempt
	Context    []ContextBundle
	// Usage is the effective set, receipt figures corrected by amendments.
	Usage        []Usage
	Verification Verification
	Artifacts    []Artifact

	// Classification and HumanOutcome are effective values: the receipt's,
	// overridden by the most recent amendment that set them.
	Classification Classification
	HumanOutcome   HumanOutcome

	// Amendments counts the corrections folded in, and Notes collects their
	// prose in application order — a run whose verdict changed should be able
	// to say why.
	Amendments int
	Notes      []string
}

// Verified reports whether the run reached a mechanically clean pass that a
// human has not since rejected or reverted. This is the denominator of
// "cost per verified result": neither an unreviewed clean pass nor an accepted
// dirty one is silently promoted.
func (v RunView) Verified() bool {
	if v.Classification != ClassificationCleanPass {
		return false
	}
	switch v.HumanOutcome {
	case HumanRejected, HumanReverted:
		return false
	}
	return true
}

type amendmentRef struct {
	record   Record
	position int
}

// Fold validates the ledger and reduces it to one view per run, newest run
// last. A ledger that does not satisfy the cross-record invariants is refused
// rather than partially folded.
func Fold(records []Record) ([]RunView, error) {
	if err := ValidateSet(records); err != nil {
		return nil, fmt.Errorf("receipt: fold: %w", err)
	}

	amendments := make(map[string][]amendmentRef)
	for i, r := range records {
		if r.RecordKind == KindRunAmendment {
			amendments[r.CorrectsReceiptID] = append(
				amendments[r.CorrectsReceiptID], amendmentRef{record: r, position: i})
		}
	}

	var views []RunView
	for _, r := range records {
		if r.RecordKind != KindRunReceipt {
			continue
		}
		views = append(views, foldOne(r, amendments[r.ReceiptID]))
	}
	return views, nil
}

func foldOne(r Record, refs []amendmentRef) RunView {
	view := RunView{
		ReceiptID:    r.ReceiptID,
		TaskID:       r.TaskID,
		RunID:        r.RunID,
		Repo:         *r.Repo,
		Policy:       *r.Policy,
		Attempts:     r.Attempts,
		Context:      r.Context,
		Usage:        append([]Usage(nil), r.Usage...),
		Verification: *r.Verification,
		Artifacts:    r.Artifacts,
		HumanOutcome: r.HumanOutcome,
	}
	if r.StartedAt != nil {
		view.StartedAt = *r.StartedAt
	}
	if r.FinishedAt != nil {
		view.FinishedAt = *r.FinishedAt
	}
	view.Classification = r.Verification.Classification

	sorted := append([]amendmentRef(nil), refs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, tj := sorted[i].record.CreatedAt, sorted[j].record.CreatedAt
		if ti != nil && tj != nil && !ti.Equal(*tj) {
			return ti.Before(*tj)
		}
		return sorted[i].position < sorted[j].position
	})

	for _, ref := range sorted {
		a := ref.record
		view.Amendments++
		if a.HumanOutcome != "" {
			view.HumanOutcome = a.HumanOutcome
		}
		if a.Classification != "" {
			view.Classification = a.Classification
		}
		if a.Note != "" {
			view.Notes = append(view.Notes, a.Note)
		}
		for _, u := range a.Usage {
			view.Usage = correctUsage(view.Usage, u)
		}
	}
	return view
}

// correctUsage replaces the figure for a metric/unit pair, appending it when
// the receipt never carried one.
func correctUsage(existing []Usage, correction Usage) []Usage {
	for i, u := range existing {
		if u.Metric == correction.Metric && u.Unit == correction.Unit {
			existing[i] = correction
			return existing
		}
	}
	return append(existing, correction)
}
