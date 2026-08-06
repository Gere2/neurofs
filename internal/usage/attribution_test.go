package usage

import (
	"context"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/runid"
)

func ownedCtx(t *testing.T) (context.Context, runid.RunID) {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, id
}

func serverCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := runid.WithAvailability(context.Background(), runid.ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestEntryAttributionRoundTrip: the JSONL encoding must preserve attribution,
// otherwise the label is written and lost in the same breath.
func TestEntryAttributionRoundTrip(t *testing.T) {
	repo := t.TempDir()
	ctx, id := ownedCtx(t)

	if _, err := AppendContext(ctx, repo, Entry{Source: "cli", Tool: "neurofs_search", Query: "auth"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := Load(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].RunID != id {
		t.Fatalf("run id lost: got %q want %q", entries[0].RunID, id)
	}
	if err := entries[0].Availability.Validate(); err != nil {
		t.Fatalf("persisted attribution invalid: %v", err)
	}
}

func TestFeedbackAttributionRoundTrip(t *testing.T) {
	repo := t.TempDir()
	ctx, id := ownedCtx(t)

	if err := AppendFeedbackContext(ctx, repo, Feedback{Query: "auth", Rating: RatingYes}); err != nil {
		t.Fatalf("append feedback: %v", err)
	}
	fbs, err := LoadFeedback(repo)
	if err != nil {
		t.Fatalf("load feedback: %v", err)
	}
	if len(fbs) != 1 || fbs[0].RunID != id {
		t.Fatalf("got %+v", fbs)
	}
}

func TestUnavailableCorrelationIsRecorded(t *testing.T) {
	repo := t.TempDir()
	ctx := serverCtx(t)

	if _, err := AppendContext(ctx, repo, Entry{Query: "auth"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := Load(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := entries[0]
	if !got.RunID.IsZero() {
		t.Fatalf("uncorrelated entry carries run id %q", got.RunID)
	}
	if got.Correlation != runid.CorrelationUnavailable || got.Reason == "" {
		t.Fatalf("gap not diagnosable from the artifact: %+v", got.Availability)
	}
}

func TestConflictingLabelIsRefused(t *testing.T) {
	repo := t.TempDir()
	ctx, _ := ownedCtx(t)
	_, other := ownedCtx(t)

	entry := Entry{Query: "auth"}
	entry.Availability = runid.Availability{RunID: other, Correlation: runid.CorrelationOwnedProcessTree}

	_, err := AppendContext(ctx, repo, entry)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite silently") {
		t.Fatalf("want conflict refusal, got %v", err)
	}
	entries, _ := Load(repo)
	if len(entries) != 0 {
		t.Fatalf("refused entry was written anyway: %+v", entries)
	}

	fb := Feedback{Query: "auth", Rating: RatingYes}
	fb.Availability = runid.Availability{RunID: other, Correlation: runid.CorrelationOwnedProcessTree}
	if err := AppendFeedbackContext(ctx, repo, fb); err == nil {
		t.Fatal("conflicting feedback label must be refused")
	}
}
