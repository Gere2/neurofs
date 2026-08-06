package quality

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/runid"
)

func readEntries(t *testing.T, repo string) []Entry {
	t.Helper()
	raw, err := os.ReadFile(Path(repo))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var out []Entry
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestRatingCarriesRunAttribution(t *testing.T) {
	repo := t.TempDir()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if err := AppendContext(ctx, repo, Entry{Query: "auth", Repo: repo, Rating: "yes"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries := readEntries(t, repo)
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

func TestUncorrelatedRatingRecordsTheGap(t *testing.T) {
	repo := t.TempDir()
	ctx, err := runid.WithAvailability(context.Background(), runid.ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendContext(ctx, repo, Entry{Query: "auth", Rating: "yes"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := readEntries(t, repo)[0]
	if !got.RunID.IsZero() || got.Correlation != runid.CorrelationUnavailable || got.Reason == "" {
		t.Fatalf("gap not diagnosable: %+v", got.Availability)
	}
}

func TestConflictingRatingLabelIsRefused(t *testing.T) {
	repo := t.TempDir()
	id, _ := runid.New()
	other, _ := runid.New()
	ctx, err := runid.NewContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	e := Entry{Query: "auth", Rating: "yes"}
	e.Availability = runid.Availability{RunID: other, Correlation: runid.CorrelationOwnedProcessTree}
	if err := AppendContext(ctx, repo, e); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite silently") {
		t.Fatalf("want conflict refusal, got %v", err)
	}
	if _, err := os.Stat(Path(repo)); !os.IsNotExist(err) {
		t.Fatalf("refused rating created the ledger, stat err = %v", err)
	}
}
