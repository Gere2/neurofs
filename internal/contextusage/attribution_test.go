package contextusage

import (
	"context"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/runid"
)

const testSession = "sess-attribution"

func sampleEntry() Entry {
	return Entry{
		SessionID:  testSession,
		Phase:      "retrieve",
		Command:    "neurofs context",
		Query:      "auth",
		BundlePath: ".neurofs/task/x.bundle.json",
		BundleHash: "3fdba35f04dc8c462986c992bcf875546257113072a909c162f7e470e581e278",
		Tokens:     1200,
	}
}

func TestEntryCarriesRunAttribution(t *testing.T) {
	repo := t.TempDir()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if err := AppendContext(ctx, repo, sampleEntry()); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := Read(repo, testSession)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	got := entries[0]
	if got.RunID != id {
		t.Fatalf("run id lost: got %q want %q", got.RunID, id)
	}
	if err := got.Availability.Validate(); err != nil {
		t.Fatalf("persisted attribution invalid: %v", err)
	}
	key := runid.JoinKey{RunID: got.RunID, BundlePath: got.BundlePath, BundleHash: got.BundleHash}
	if err := key.Validate(); err != nil {
		t.Fatalf("join key does not survive persistence: %v", err)
	}
}

func TestUncorrelatedEntryRecordsTheGap(t *testing.T) {
	repo := t.TempDir()
	ctx, err := runid.WithAvailability(context.Background(), runid.ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendContext(ctx, repo, sampleEntry()); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := Read(repo, testSession)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := entries[0]
	if !got.RunID.IsZero() || got.Correlation != runid.CorrelationUnavailable || got.Reason == "" {
		t.Fatalf("gap not diagnosable: %+v", got.Availability)
	}
}

func TestConflictingEntryLabelIsRefused(t *testing.T) {
	repo := t.TempDir()
	id, _ := runid.New()
	other, _ := runid.New()
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	e := sampleEntry()
	e.Availability = runid.Availability{RunID: other, Correlation: runid.CorrelationOwnedProcessTree}
	if err := AppendContext(ctx, repo, e); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite silently") {
		t.Fatalf("want conflict refusal, got %v", err)
	}
	entries, _ := Read(repo, testSession)
	if len(entries) != 0 {
		t.Fatalf("refused entry was written anyway: %+v", entries)
	}
}
