package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/runid"
)

const testBundleHash = "3fdba35f04dc8c462986c992bcf875546257113072a909c162f7e470e581e278"

func mustRunID(t *testing.T) runid.RunID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("new run id: %v", err)
	}
	return id
}

// ownedCtx is a context labelled as a run NeuroFS controls.
func ownedCtx(t *testing.T, id runid.RunID) context.Context {
	t.Helper()
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatalf("new run context: %v", err)
	}
	return ctx
}

// serverCtx is a context that explicitly declares correlation unavailable —
// the persistent-server topology.
func serverCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := runid.WithAvailability(context.Background(), runid.ForPersistentServer())
	if err != nil {
		t.Fatalf("new server context: %v", err)
	}
	return ctx
}

func sampleEntry() models.LedgerEntry {
	return models.LedgerEntry{
		Timestamp:  time.Now().UTC().Truncate(time.Second),
		SessionID:  "sess-attribution",
		Query:      "how does auth work",
		BundlePath: ".neurofs/task/x.bundle.json",
		BundleHash: testBundleHash,
		Files:      []string{"internal/auth/auth.go"},
		Command:    "neurofs ask",
		Outcome:    "ok",
	}
}

// stores exercises every Store implementation through the same contract, so a
// backend cannot quietly drop attribution the others keep.
func stores(t *testing.T) map[string]Store {
	t.Helper()
	return map[string]Store{
		"mem":    NewMemStore(),
		"file":   NewFileStore(t.TempDir()),
		"sqlite": NewSqliteStore(t.TempDir()),
	}
}

func TestAppendLabelsEntryWithRunIdentity(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			id := mustRunID(t)
			ctx := ownedCtx(t, id)

			if err := store.Append(ctx, sampleEntry()); err != nil {
				t.Fatalf("append: %v", err)
			}
			entries, err := store.Read(ctx, "")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("got %d entries, want 1", len(entries))
			}

			got := entries[0]
			if got.RunID != id {
				t.Fatalf("run id lost in round trip: got %q, want %q", got.RunID, id)
			}
			if got.Correlation != runid.CorrelationOwnedProcessTree {
				t.Fatalf("got correlation %q", got.Correlation)
			}
			if err := got.Availability.Validate(); err != nil {
				t.Fatalf("persisted attribution is invalid: %v", err)
			}
			// Point 7: the join key must survive persistence intact.
			key := runid.JoinKey{RunID: got.RunID, BundlePath: got.BundlePath, BundleHash: got.BundleHash}
			if err := key.Validate(); err != nil {
				t.Fatalf("join key does not survive the round trip: %v", err)
			}
		})
	}
}

func TestAppendRecordsUnavailableCorrelation(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := serverCtx(t)
			if err := store.Append(ctx, sampleEntry()); err != nil {
				t.Fatalf("append: %v", err)
			}
			entries, err := store.Read(ctx, "")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			got := entries[0]

			if !got.RunID.IsZero() {
				t.Fatalf("unavailable entry carries run id %q", got.RunID)
			}
			if got.Correlation != runid.CorrelationUnavailable {
				t.Fatalf("got correlation %q", got.Correlation)
			}
			// The gap must be diagnosable from the artifact itself.
			if got.Reason == "" {
				t.Fatal("unavailable entry persisted without a reason")
			}
			if err := got.Availability.Validate(); err != nil {
				t.Fatalf("persisted attribution is invalid: %v", err)
			}
		})
	}
}

// TestAppendRefusesConflictingLabel: auto-labelling must never become a way to
// silently overwrite or forge an attribution.
func TestAppendRefusesConflictingLabel(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := ownedCtx(t, mustRunID(t))

			entry := sampleEntry()
			entry.Availability = runid.Availability{
				RunID:       mustRunID(t), // a different run
				Correlation: runid.CorrelationOwnedProcessTree,
			}
			err := store.Append(ctx, entry)
			if err == nil {
				t.Fatal("appending an entry labelled for another run must fail")
			}
			if !strings.Contains(err.Error(), "refusing to overwrite silently") {
				t.Fatalf("unexpected error: %v", err)
			}

			entries, readErr := store.Read(ctx, "")
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("rejected entry was written anyway: %+v", entries)
			}
		})
	}
}

func TestAppendRefusesInvalidLabel(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			entry := sampleEntry()
			// Unavailable without a reason: an undiagnosable gap.
			entry.Availability = runid.Availability{Correlation: runid.CorrelationUnavailable}
			if err := store.Append(ownedCtx(t, mustRunID(t)), entry); err == nil {
				t.Fatal("invalid attribution must be rejected")
			}
		})
	}
}

// TestAppendMatchingLabelIsAccepted: re-appending an entry that already carries
// the current run's attribution is legitimate (replay, migration).
func TestAppendMatchingLabelIsAccepted(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			id := mustRunID(t)
			ctx := ownedCtx(t, id)

			entry := sampleEntry()
			entry.Availability = runid.Availability{
				RunID:       id,
				Correlation: runid.CorrelationOwnedProcessTree,
			}
			if err := store.Append(ctx, entry); err != nil {
				t.Fatalf("matching attribution rejected: %v", err)
			}
			entries, err := store.Read(ctx, "")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(entries) != 1 || entries[0].RunID != id {
				t.Fatalf("got %+v", entries)
			}
		})
	}
}

// TestFileStoreJSONShape pins the on-disk field names: the ledger is history,
// and renaming a persisted key silently orphans every existing entry.
func TestFileStoreJSONShape(t *testing.T) {
	repo := t.TempDir()
	store := NewFileStore(repo)
	id := mustRunID(t)

	if err := store.Append(ownedCtx(t, id), sampleEntry()); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".neurofs", "ledger.jsonl"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	var fields map[string]any
	line := strings.TrimSpace(strings.Split(string(raw), "\n")[0])
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]any{
		"run_id":          id.String(),
		"run_correlation": string(runid.CorrelationOwnedProcessTree),
		"bundle_path":     ".neurofs/task/x.bundle.json",
		"bundle_hash":     testBundleHash,
	} {
		got, ok := fields[key]
		if !ok {
			t.Errorf("field %q missing from the persisted entry", key)
			continue
		}
		if got != want {
			t.Errorf("field %q: got %v, want %v", key, got, want)
		}
	}
	// An available correlation has nothing to explain.
	if _, ok := fields["run_correlation_reason"]; ok {
		t.Error("correlated entry persisted an empty reason field")
	}
}

// TestLegacyEntriesReadAsUnlabelled: rows written before run correlation have
// empty columns. They must read back as a zero Availability — indistinguishable
// from unlabelled — never as a malformed one that poisons later validation.
func TestLegacyEntriesReadAsUnlabelled(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		repo := t.TempDir()
		dir := filepath.Join(repo, ".neurofs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		legacy := `{"timestamp":"2026-01-02T03:04:05Z","session_id":"old","query":"legacy","bundle_hash":"deadbeef"}`
		if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(legacy+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertLegacyReadable(t, NewFileStore(repo))
	})

	t.Run("sqlite", func(t *testing.T) {
		store := NewSqliteStore(t.TempDir())
		ctx := serverCtx(t)
		// Materialize the schema, then blank the correlation columns the way a
		// pre-migration row would look.
		if err := store.Append(ctx, sampleEntry()); err != nil {
			t.Fatal(err)
		}
		db, err := store.openDB(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(ctx,
			`UPDATE session_ledger SET run_id='', run_correlation='', run_correlation_reason='', bundle_path=''`)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		assertLegacyReadable(t, store)
	})
}

func assertLegacyReadable(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	entries, err := store.Read(ctx, "")
	if err != nil {
		t.Fatalf("reading a legacy ledger must not fail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Availability != (runid.Availability{}) {
		t.Fatalf("legacy entry read back as %+v, want the zero attribution", entries[0].Availability)
	}

	// A legacy entry must be re-labellable rather than permanently poisoned.
	id := mustRunID(t)
	relabelled := entries[0]
	if err := store.Append(ownedCtx(t, id), relabelled); err != nil {
		t.Fatalf("re-appending a legacy entry must succeed: %v", err)
	}
}
