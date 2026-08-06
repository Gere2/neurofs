package receipt

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func openIndex(t *testing.T, repo string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", IndexPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedLedger appends a receipt and its amendment through the real store, so
// the index is always built from a ledger that passed verification.
func seedLedger(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	r := validReceipt(t)
	if err := AppendRecord(repo, &r); err != nil {
		t.Fatal(err)
	}
	a := validAmendment(t)
	if err := AppendRecord(repo, &a); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestRebuildIndex(t *testing.T) {
	repo := seedLedger(t)
	ctx := context.Background()

	stats, err := RebuildIndex(ctx, repo)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.Runs != 1 || stats.VerifiedRuns != 1 {
		t.Fatalf("stats: %+v", stats)
	}

	db := openIndex(t, repo)
	var (
		classification, humanOutcome, models, surfaces string
		verified, amendments                           int
	)
	err = db.QueryRow(`SELECT classification, human_outcome, verified, amendments, surfaces, models
	                   FROM runs`).
		Scan(&classification, &humanOutcome, &verified, &amendments, &surfaces, &models)
	if err != nil {
		t.Fatalf("query run: %v", err)
	}
	if classification != string(ClassificationCleanPass) {
		t.Fatalf("classification %q", classification)
	}
	// The amendment's human outcome must be folded in, not the receipt's.
	if humanOutcome != string(HumanAccepted) {
		t.Fatalf("human_outcome %q: the amendment was not folded", humanOutcome)
	}
	if verified != 1 || amendments != 1 {
		t.Fatalf("verified=%d amendments=%d", verified, amendments)
	}
	if surfaces != "copilot_cli" {
		t.Fatalf("surfaces %q", surfaces)
	}
	// A router that switched models mid-session must not be collapsed to one.
	if !strings.Contains(models, "claude-sonnet-5") || !strings.Contains(models, "gpt-5.4-mini") {
		t.Fatalf("models %q: mid-session switch lost", models)
	}
}

// TestIndexStoresQuantitiesAsText: a REAL column would push every cost through
// binary floating point and make the index disagree with the ledger.
func TestIndexStoresQuantitiesAsText(t *testing.T) {
	repo := seedLedger(t)
	if _, err := RebuildIndex(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	db := openIndex(t, repo)

	var declType string
	err := db.QueryRow(`SELECT type FROM pragma_table_info('run_usage') WHERE name='quantity'`).Scan(&declType)
	if err != nil {
		t.Fatalf("inspect column: %v", err)
	}
	if !strings.EqualFold(declType, "TEXT") {
		t.Fatalf("quantity column is %q, want TEXT — a numeric column reintroduces float error", declType)
	}

	// The amendment corrected premium_requests from unknown to a reported 1.
	var quantity, provenance string
	err = db.QueryRow(`SELECT quantity, provenance FROM run_usage WHERE metric='premium_requests'`).
		Scan(&quantity, &provenance)
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if quantity != "1" || provenance != string(ProvenanceProviderReported) {
		t.Fatalf("usage correction not indexed: quantity=%q provenance=%q", quantity, provenance)
	}
}

// TestRebuildIndexIsIdempotentAndDroppable: the index is a cache. Rebuilding
// must not accumulate, and deleting it must cost nothing.
func TestRebuildIndexIsIdempotentAndDroppable(t *testing.T) {
	repo := seedLedger(t)
	ctx := context.Background()

	first, err := RebuildIndex(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RebuildIndex(ctx, repo)
	if err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if first != second {
		t.Fatalf("rebuild accumulated: %+v then %+v", first, second)
	}

	if err := os.Remove(IndexPath(repo)); err != nil {
		t.Fatal(err)
	}
	third, err := RebuildIndex(ctx, repo)
	if err != nil {
		t.Fatalf("rebuild after delete: %v", err)
	}
	if third != first {
		t.Fatalf("rebuild from scratch differs: %+v vs %+v", third, first)
	}
}

// TestRebuildIndexRefusesCorruptedLedger: the index must never launder a
// history that failed verification into a clean-looking table.
func TestRebuildIndexRefusesCorruptedLedger(t *testing.T) {
	repo := seedLedger(t)
	ctx := context.Background()
	if _, err := RebuildIndex(ctx, repo); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(LedgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `"human_outcome":"unreviewed"`, `"human_outcome":"accepted"`, 1)
	if edited == string(raw) {
		t.Fatal("test setup: edit did not apply")
	}
	if err := os.WriteFile(LedgerPath(repo), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RebuildIndex(ctx, repo); err == nil ||
		!strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Fatalf("want refusal on a tampered ledger, got %v", err)
	}
}

func TestRebuildIndexOnEmptyLedger(t *testing.T) {
	repo := t.TempDir()
	stats, err := RebuildIndex(context.Background(), repo)
	if err != nil {
		t.Fatalf("an absent ledger must index as empty: %v", err)
	}
	if stats.Runs != 0 || stats.UsageRows != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	db := openIndex(t, repo)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM runs`).Scan(&n); err != nil {
		t.Fatalf("the schema must exist even with no rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("got %d rows", n)
	}
}
