package audit

import "sort"

// SetDiff is the before/after delta for one drift bucket.
// Added = in B, not in A. Removed = in A, not in B. The convention
// keeps the rest of the CLI readable: "added" is always the new side.
type SetDiff struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// Diff is the full comparison between two audit records. Scalars use B−A so
// positive numbers mean "B is higher than A" — readers interpret good/bad
// from the metric name, not the sign.
//
// RecallApplies is true only when at least one record carries ExpectsFacts.
// When both records skipped facts, RecallDelta is meaningless and callers
// should hide it rather than show a misleading 0.0%.
type Diff struct {
	A AuditRecord `json:"-"`
	B AuditRecord `json:"-"`

	SameBundle   bool `json:"same_bundle"`
	SameQuestion bool `json:"same_question"`
	SameModel    bool `json:"same_model"`

	// SameMode is true only when both records carry a non-empty mode label
	// and the labels match. An empty mode on either side flips SameMode to
	// false — "no label" is not the same as "both labelled strategy".
	SameMode bool   `json:"same_mode"`
	ModeA    string `json:"mode_a,omitempty"`
	ModeB    string `json:"mode_b,omitempty"`

	// Human annotations on both sides. Pass-through for display; we don't
	// try to diff free-form prose. Empty strings render as "—" in the UI.
	TitleA string `json:"title_a,omitempty"`
	TitleB string `json:"title_b,omitempty"`
	BriefA string `json:"brief_a,omitempty"`
	BriefB string `json:"brief_b,omitempty"`
	NoteA  string `json:"note_a,omitempty"`
	NoteB  string `json:"note_b,omitempty"`

	GroundedDelta float64 `json:"grounded_delta"`
	DriftDelta    float64 `json:"drift_delta"`
	RecallDelta   float64 `json:"recall_delta"`
	RecallApplies bool    `json:"recall_applies"`

	Paths   SetDiff `json:"paths"`
	APIs    SetDiff `json:"apis"`
	Symbols SetDiff `json:"symbols"`
}

// DiffRecords computes the full delta between two records. It never errors —
// missing fields produce zero-valued deltas, which is accurate for the
// "nothing to compare" case. The function is pure; callers own I/O.
func DiffRecords(a, b AuditRecord) Diff {
	d := Diff{
		A:             a,
		B:             b,
		SameBundle:    a.BundleHash == b.BundleHash && a.BundleHash != "",
		SameQuestion:  a.Question == b.Question,
		SameModel:     a.Model == b.Model,
		SameMode:      a.Mode == b.Mode && a.Mode != "",
		ModeA:         a.Mode,
		ModeB:         b.Mode,
		TitleA:        a.Title,
		TitleB:        b.Title,
		BriefA:        a.Brief,
		BriefB:        b.Brief,
		NoteA:         a.Note,
		NoteB:         b.Note,
		GroundedDelta: b.GroundedRatio - a.GroundedRatio,
		DriftDelta:    b.Drift.Rate - a.Drift.Rate,
	}
	if len(a.ExpectsFacts) > 0 || len(b.ExpectsFacts) > 0 {
		d.RecallApplies = true
		d.RecallDelta = b.AnswerRecall - a.AnswerRecall
	}
	d.Paths = diffSets(a.Drift.UnknownPaths, b.Drift.UnknownPaths)
	d.APIs = diffSets(a.Drift.UnknownAPIs, b.Drift.UnknownAPIs)
	d.Symbols = diffSets(a.Drift.UnknownSymbols, b.Drift.UnknownSymbols)
	return d
}

// diffSets returns the added/removed lists between two string slices. Empty
// slices are fine and produce a SetDiff with nil fields, which omitempty
// renders cleanly in JSON.
func diffSets(as, bs []string) SetDiff {
	aSet := toSet(as)
	bSet := toSet(bs)

	var added, removed []string
	for x := range bSet {
		if !aSet[x] {
			added = append(added, x)
		}
	}
	for x := range aSet {
		if !bSet[x] {
			removed = append(removed, x)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return SetDiff{Added: added, Removed: removed}
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
