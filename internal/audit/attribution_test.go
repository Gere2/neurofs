package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/runid"
)

func sampleRecord() AuditRecord {
	return AuditRecord{
		Question:   "how does auth work",
		Model:      "stub",
		BundlePath: ".neurofs/task/x.bundle.json",
		BundleHash: "3fdba35f04dc8c462986c992bcf875546257113072a909c162f7e470e581e278",
		Response:   "auth is handled in internal/auth",
	}
}

func TestRecordCarriesRunAttribution(t *testing.T) {
	dir := t.TempDir()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	path, err := SaveRecordContext(ctx, dir, sampleRecord())
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadRecord(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.RunID != id {
		t.Fatalf("run id lost: got %q want %q", got.RunID, id)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("persisted attribution invalid: %v", err)
	}
	// An audit verdict is evidence about a specific bundle; the join must hold.
	key := runid.JoinKey{RunID: got.RunID, BundlePath: got.BundlePath, BundleHash: got.BundleHash}
	if err := key.Validate(); err != nil {
		t.Fatalf("join key does not survive persistence: %v", err)
	}
}

func TestUncorrelatedRecordRecordsTheGap(t *testing.T) {
	dir := t.TempDir()
	ctx, err := runid.WithAvailability(context.Background(), runid.ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}
	path, err := SaveRecordContext(ctx, dir, sampleRecord())
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadRecord(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.RunID.IsZero() || got.Correlation != runid.CorrelationUnavailable || got.Reason == "" {
		t.Fatalf("gap not diagnosable: %+v", got.Availability)
	}
}

func TestConflictingRecordLabelIsRefused(t *testing.T) {
	dir := t.TempDir()
	id, _ := runid.New()
	other, _ := runid.New()
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	rec := sampleRecord()
	rec.Availability = runid.Availability{RunID: other, Correlation: runid.CorrelationOwnedProcessTree}

	if _, err := SaveRecordContext(ctx, dir, rec); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite silently") {
		t.Fatalf("want conflict refusal, got %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Fatalf("refused record was written anyway: %s", e.Name())
		}
	}
}
