package grounding

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func bundle() models.Bundle {
	return models.Bundle{
		Query:      "how does auth work",
		BundleHash: "abc123",
		Fragments: []models.ContextFragment{
			{RelPath: "src/auth.ts", Content: "export function verifyToken(t: string) { return jwtVerify(t) }"},
			{RelPath: "src/user.ts", Content: "export class UserRepository { findById(id) {} }"},
		},
	}
}

func TestScoreEditInContext(t *testing.T) {
	e := ScoreEdit(bundle(), "src/auth.ts", "function verifyToken() { return true }")
	if e.Kind != KindEdit {
		t.Fatalf("kind = %q", e.Kind)
	}
	if e.FileInContext == nil || !*e.FileInContext {
		t.Fatalf("expected FileInContext true for a file in the bundle")
	}
	if !e.Grounded() {
		t.Fatalf("an edit on a context file should be grounded")
	}
}

func TestScoreEditOutsideContext(t *testing.T) {
	e := ScoreEdit(bundle(), "src/brand_new.ts", "const x = 1")
	if e.FileInContext == nil || *e.FileInContext {
		t.Fatalf("expected FileInContext false for a file not in the bundle")
	}
	if e.Grounded() {
		t.Fatalf("an edit outside context should not be grounded")
	}
	if e.Note == "" {
		t.Fatalf("expected a note explaining the out-of-context edit")
	}
}

func TestScoreEditNoBundle(t *testing.T) {
	e := ScoreEdit(models.Bundle{}, "src/auth.ts", "code")
	if e.FileInContext == nil || *e.FileInContext {
		t.Fatalf("with no bundle the file cannot be in context")
	}
	if e.Note == "" {
		t.Fatalf("expected a note about the missing bundle")
	}
}

func TestScoreResponseGroundedCitation(t *testing.T) {
	resp := "Authentication is handled in src/auth.ts:1 via verifyToken."
	e := ScoreResponse(bundle(), resp)
	if e.Kind != KindResponse {
		t.Fatalf("kind = %q", e.Kind)
	}
	if e.GroundedRatio < 0.999 {
		t.Fatalf("grounded ratio = %.2f, want 1.0 (the cited file is in the bundle)", e.GroundedRatio)
	}
	found := false
	for _, f := range e.Files {
		if f == "src/auth.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected src/auth.ts among cited files, got %v", e.Files)
	}
}

func TestScoreResponseDriftOnInvention(t *testing.T) {
	resp := "The flow goes through PhantomController and ImaginaryService which call verifyToken."
	e := ScoreResponse(bundle(), resp)
	if e.DriftRate <= 0 {
		t.Fatalf("expected drift > 0 for invented identifiers, got %.2f", e.DriftRate)
	}
}

func TestAppendReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	in := bundle()
	e1 := ScoreEdit(in, "src/auth.ts", "x")
	e1.Origin = "PostToolUse:Edit"
	e2 := ScoreResponse(in, "see src/user.ts:1")
	e2.Origin = "Stop"
	if err := Append(dir, e1); err != nil {
		t.Fatal(err)
	}
	if err := Append(dir, e2); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d events, want 2", len(got))
	}
	if got[0].Origin != "PostToolUse:Edit" || got[1].Origin != "Stop" {
		t.Fatalf("origins not preserved: %q %q", got[0].Origin, got[1].Origin)
	}
}

func TestReadMissingIsEmpty(t *testing.T) {
	got, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("missing ledger should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no events, got %d", len(got))
	}
}

func TestSummarize(t *testing.T) {
	tru, fls := true, false
	events := []Event{
		{Kind: KindEdit, FileInContext: &tru, DriftRate: 0.2},
		{Kind: KindEdit, FileInContext: &fls, DriftRate: 0.4},
		{Kind: KindResponse, GroundedRatio: 1.0, DriftRate: 0.1},
		{Kind: KindResponse, GroundedRatio: 0.2, DriftRate: 0.6},
	}
	a := Summarize(events)
	if a.Events != 4 || a.Edits != 2 || a.Responses != 2 {
		t.Fatalf("counts off: %+v", a)
	}
	if a.EditsInContext != 1 || a.EditCoverage < 0.49 || a.EditCoverage > 0.51 {
		t.Fatalf("edit coverage off: %+v", a)
	}
	if a.MeanGroundedResp < 0.59 || a.MeanGroundedResp > 0.61 {
		t.Fatalf("mean grounded resp = %.2f, want ~0.6", a.MeanGroundedResp)
	}
	// Concerning: one out-of-context edit + one poorly grounded response.
	if a.Concerning != 2 {
		t.Fatalf("concerning = %d, want 2", a.Concerning)
	}
}
