package grounding

import (
	"context"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/runid"
)

const testBundleHash = "3fdba35f04dc8c462986c992bcf875546257113072a909c162f7e470e581e278"

func sampleEvent() Event {
	return Event{
		Origin:     "Stop",
		Kind:       "response",
		BundlePath: ".neurofs/task/x.bundle.json",
		BundleHash: testBundleHash,
	}
}

func TestEventCarriesRunAttribution(t *testing.T) {
	repo := t.TempDir()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if err := AppendContext(ctx, repo, sampleEvent()); err != nil {
		t.Fatalf("append: %v", err)
	}
	events, err := Read(repo)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	got := events[0]
	if got.RunID != id {
		t.Fatalf("run id lost: got %q want %q", got.RunID, id)
	}
	if err := got.Availability.Validate(); err != nil {
		t.Fatalf("persisted attribution invalid: %v", err)
	}
	// Grounding evidence is only useful if it can be joined back to the exact
	// bundle it was scored against.
	key := runid.JoinKey{RunID: got.RunID, BundlePath: got.BundlePath, BundleHash: got.BundleHash}
	if err := key.Validate(); err != nil {
		t.Fatalf("join key does not survive persistence: %v", err)
	}
}

func TestUncorrelatedEventRecordsTheGap(t *testing.T) {
	repo := t.TempDir()
	ctx, err := runid.WithAvailability(context.Background(), runid.ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendContext(ctx, repo, sampleEvent()); err != nil {
		t.Fatalf("append: %v", err)
	}
	events, err := Read(repo)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := events[0]
	if !got.RunID.IsZero() || got.Correlation != runid.CorrelationUnavailable || got.Reason == "" {
		t.Fatalf("gap not diagnosable: %+v", got.Availability)
	}
}

func TestConflictingEventLabelIsRefused(t *testing.T) {
	repo := t.TempDir()
	id, _ := runid.New()
	other, _ := runid.New()
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	e := sampleEvent()
	e.Availability = runid.Availability{RunID: other, Correlation: runid.CorrelationOwnedProcessTree}
	if err := AppendContext(ctx, repo, e); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite silently") {
		t.Fatalf("want conflict refusal, got %v", err)
	}
	events, _ := Read(repo)
	if len(events) != 0 {
		t.Fatalf("refused event was written anyway: %+v", events)
	}
}
