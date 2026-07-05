// Package grounding turns audit grounding from a paste-the-answer-by-hand
// step into a continuous, automatic signal for autonomous loops.
//
// A Claude Code hook (PostToolUse on Edit/Write, or Stop) pipes the agent's
// action to `neurofs ground`, which scores it against the context bundle the
// agent was given and appends an Event to an append-only ledger
// (audit/grounding.jsonl). `neurofs stats` and `neurofs ground --feed` then
// surface the rolling aggregate so a human supervising a long loop can trust it
// without reading every diff — "CI of grounding".
//
// Two complementary signals, both leaning on internal/audit:
//
//	edit     — did the edit land on a file the agent actually had context for,
//	           and how much of what it wrote is anchored in that context?
//	response — the existing audit grounding (valid citations, drift) over the
//	           agent's prose, automated instead of pasted.
//
// Every Event carries its origin, timestamp, session, and the files involved —
// the ledger is auditable, never a black box.
package grounding

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/models"
)

// Kind classifies a grounding observation.
const (
	KindEdit     = "edit"
	KindResponse = "response"
)

// Event is one continuous-grounding observation. It is the persistence unit
// the supervisor feed aggregates.
type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"session_id,omitempty"`
	Origin      string    `json:"origin"` // e.g. "PostToolUse:Edit", "Stop", "manual"
	Kind        string    `json:"kind"`   // KindEdit | KindResponse
	BundleHash  string    `json:"bundle_hash,omitempty"`
	BundleQuery string    `json:"bundle_query,omitempty"`
	Files       []string  `json:"files,omitempty"` // edited files, or cited files for a response

	// Edit-kind only: was the edited file part of the context the agent had?
	FileInContext *bool `json:"file_in_context,omitempty"`

	// Grounding metrics, audit-derived.
	GroundedRatio float64  `json:"grounded_ratio"`         // response kind: valid citations / total
	DriftRate     float64  `json:"drift_rate"`             // unknown / (known + unknown)
	UnknownRefs   []string `json:"unknown_refs,omitempty"` // sample of drifted identifiers

	Note string `json:"note,omitempty"`
}

// Grounded reports whether this event clears a simple per-event bar: for an
// edit, the file was in context; for a response, grounded >= 0.5 and drift is
// not dominant. It is a coarse flag for the feed's ✓/· marks, not the verdict.
func (e Event) Grounded() bool {
	switch e.Kind {
	case KindEdit:
		return e.FileInContext != nil && *e.FileInContext
	case KindResponse:
		return e.GroundedRatio >= 0.5 && e.DriftRate < 0.5
	default:
		return false
	}
}

// Path is the repo-local grounding ledger. It sits next to audit/records so it
// travels with the repo as a shareable artefact if the user commits it.
func Path(repoRoot string) string {
	return filepath.Join(repoRoot, "audit", "grounding.jsonl")
}

// Append writes one event to the ledger, creating it if missing.
func Append(repoRoot string, e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	p := Path(repoRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(enc, '\n'))
	return err
}

// Read loads every event from the ledger. A missing file is "no events yet",
// not an error.
func Read(repoRoot string) ([]Event, error) {
	p := Path(repoRoot)
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("grounding: decode %s: %w", p, err)
		}
		events = append(events, e)
	}
	return events, sc.Err()
}

// ScoreEdit grounds an agent edit against the context bundle it was given:
// whether the edited file was in that context, and how much of the text the
// agent ADDED is anchored in the bundle (drift over the added text). High edit
// drift can be legitimate new code, so it is informational — the load-bearing
// signal is FileInContext.
func ScoreEdit(bundle models.Bundle, editedRel, addedText string) Event {
	editedRel = normPath(editedRel)
	inCtx := fileInBundle(bundle, editedRel)
	e := Event{
		Kind:          KindEdit,
		Files:         []string{editedRel},
		FileInContext: &inCtx,
		BundleHash:    bundle.BundleHash,
		BundleQuery:   bundle.Query,
	}
	if strings.TrimSpace(addedText) != "" && len(bundle.Fragments) > 0 {
		drift := audit.DetectDrift(addedText, bundle)
		e.DriftRate = drift.Rate
		e.UnknownRefs = sampleUnknown(drift, 6)
	}
	if len(bundle.Fragments) == 0 {
		e.Note = "no context bundle available to ground against"
	} else if !inCtx {
		e.Note = "edit landed on a file outside the provided context"
	}
	return e
}

// ScoreResponse grounds an agent's prose response against the bundle, reusing
// the exact audit pipeline `audit replay` uses (citation validation + drift).
// This is the automated form of paste-the-answer replay.
func ScoreResponse(bundle models.Bundle, response string) Event {
	citations := audit.ValidateCitations(audit.ParseCitations(response), bundle)
	drift := audit.DetectDrift(response, bundle)
	e := Event{
		Kind:          KindResponse,
		BundleHash:    bundle.BundleHash,
		BundleQuery:   bundle.Query,
		GroundedRatio: audit.GroundedRatio(citations),
		DriftRate:     drift.Rate,
		UnknownRefs:   sampleUnknown(drift, 6),
	}
	for _, c := range citations {
		if c.Valid {
			e.Files = appendUnique(e.Files, c.RelPath)
		}
	}
	if len(bundle.Fragments) == 0 {
		e.Note = "no context bundle available to ground against"
	}
	return e
}

// Aggregate is the rolled-up supervisor view of the grounding ledger.
type Aggregate struct {
	Events int `json:"events"`

	Edits          int     `json:"edits"`
	EditsInContext int     `json:"edits_in_context"`
	EditCoverage   float64 `json:"edit_coverage"` // edits_in_context / edits
	MeanEditDrift  float64 `json:"mean_edit_drift"`

	Responses        int     `json:"responses"`
	MeanGroundedResp float64 `json:"mean_grounded_response"`
	MeanRespDrift    float64 `json:"mean_response_drift"`

	// Concerning is the count of events that did not clear the per-event bar
	// (an edit outside context, or a poorly grounded response).
	Concerning int `json:"concerning"`
}

// Summarize condenses events into the aggregate the feed and stats render.
func Summarize(events []Event) Aggregate {
	a := Aggregate{Events: len(events)}
	var editDrift, respGround, respDrift float64
	for _, e := range events {
		if !e.Grounded() {
			a.Concerning++
		}
		switch e.Kind {
		case KindEdit:
			a.Edits++
			if e.FileInContext != nil && *e.FileInContext {
				a.EditsInContext++
			}
			editDrift += e.DriftRate
		case KindResponse:
			a.Responses++
			respGround += e.GroundedRatio
			respDrift += e.DriftRate
		}
	}
	if a.Edits > 0 {
		a.EditCoverage = float64(a.EditsInContext) / float64(a.Edits)
		a.MeanEditDrift = editDrift / float64(a.Edits)
	}
	if a.Responses > 0 {
		a.MeanGroundedResp = respGround / float64(a.Responses)
		a.MeanRespDrift = respDrift / float64(a.Responses)
	}
	return a
}

func fileInBundle(bundle models.Bundle, rel string) bool {
	for _, f := range bundle.Fragments {
		if normPath(f.RelPath) == rel {
			return true
		}
	}
	return false
}

func sampleUnknown(d audit.DriftReport, limit int) []string {
	var all []string
	all = append(all, d.UnknownPaths...)
	all = append(all, d.UnknownAPIs...)
	all = append(all, d.UnknownSymbols...)
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func appendUnique(xs []string, v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return xs
	}
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func normPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
}
