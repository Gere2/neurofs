package receipt

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// IndexRelPath is the derived index, relative to the repo root.
//
// The index is a cache, never a source. It is dropped and rebuilt in full from
// audit/run_receipts.jsonl on every rebuild, so it can be deleted at any time
// without losing anything, and no incremental state exists that could silently
// drift from the ledger. Nothing reads it to make a decision the ledger could
// not answer on its own.
const IndexRelPath = "audit/run_receipts.index.db"

// IndexPath returns the absolute path of the derived index for a repo root.
func IndexPath(repoRoot string) string {
	return filepath.Join(repoRoot, "audit", "run_receipts.index.db")
}

// IndexStats reports what a rebuild wrote.
type IndexStats struct {
	Runs         int
	VerifiedRuns int
	UsageRows    int
}

// Quantities are stored as TEXT, deliberately. A REAL column would route every
// cost and credit figure through binary floating point and undo the exact
// decimal arithmetic the rest of this package is careful to preserve — the
// index would then disagree with the ledger it was derived from.
const indexSchema = `
DROP TABLE IF EXISTS run_usage;
DROP TABLE IF EXISTS runs;
CREATE TABLE runs (
	receipt_id         TEXT PRIMARY KEY,
	task_id            TEXT NOT NULL,
	run_id             TEXT NOT NULL UNIQUE,
	started_at         TEXT NOT NULL,
	finished_at        TEXT NOT NULL,
	repo_identity      TEXT NOT NULL,
	base_commit        TEXT NOT NULL,
	policy_enforcement TEXT NOT NULL,
	policy_decision    TEXT NOT NULL,
	classification     TEXT NOT NULL,
	human_outcome      TEXT NOT NULL,
	verified           INTEGER NOT NULL,
	amendments         INTEGER NOT NULL,
	attempts           INTEGER NOT NULL,
	surfaces           TEXT NOT NULL,
	models             TEXT NOT NULL
);
CREATE TABLE run_usage (
	receipt_id TEXT NOT NULL REFERENCES runs(receipt_id),
	metric     TEXT NOT NULL,
	unit       TEXT NOT NULL,
	quantity   TEXT NOT NULL,
	provenance TEXT NOT NULL,
	confidence TEXT NOT NULL,
	PRIMARY KEY (receipt_id, metric, unit)
);
CREATE INDEX idx_runs_task ON runs (task_id);
CREATE INDEX idx_runs_verified ON runs (verified, classification);
CREATE INDEX idx_usage_metric ON run_usage (metric, unit);
`

// RebuildIndex reloads the ledger, folds it, and replaces the derived index
// with the result. The ledger is verified on load, so a corrupted or edited
// history refuses to produce an index rather than yielding a plausible one.
func RebuildIndex(ctx context.Context, repoRoot string) (IndexStats, error) {
	records, err := LoadLedger(repoRoot)
	if err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: %w", err)
	}
	views, err := Fold(records)
	if err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: %w", err)
	}

	path := IndexPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000;`); err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: pragma: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, indexSchema); err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: schema: %w", err)
	}

	stats := IndexStats{}
	for _, v := range views {
		surfaces, models := attemptSummary(v.Attempts)
		verified := 0
		if v.Verified() {
			verified = 1
			stats.VerifiedRuns++
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO runs (
				receipt_id, task_id, run_id, started_at, finished_at,
				repo_identity, base_commit, policy_enforcement, policy_decision,
				classification, human_outcome, verified, amendments, attempts,
				surfaces, models
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			v.ReceiptID, v.TaskID, v.RunID,
			v.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
			v.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"),
			v.Repo.Identity, v.Repo.BaseCommit,
			string(v.Policy.Enforcement), string(v.Policy.Decision),
			string(v.Classification), string(v.HumanOutcome),
			verified, v.Amendments, len(v.Attempts),
			strings.Join(surfaces, ","), strings.Join(models, ","),
		); err != nil {
			return IndexStats{}, fmt.Errorf("receipt: rebuild index: insert run %s: %w", v.ReceiptID, err)
		}
		stats.Runs++

		for _, u := range v.Usage {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_usage (receipt_id, metric, unit, quantity, provenance, confidence)
				VALUES (?,?,?,?,?,?)`,
				v.ReceiptID, u.Metric, u.Unit, u.Quantity,
				string(u.Provenance), string(u.Confidence),
			); err != nil {
				return IndexStats{}, fmt.Errorf("receipt: rebuild index: insert usage for %s: %w", v.ReceiptID, err)
			}
			stats.UsageRows++
		}
	}

	if err := tx.Commit(); err != nil {
		return IndexStats{}, fmt.Errorf("receipt: rebuild index: commit: %w", err)
	}
	committed = true
	return stats, nil
}

// attemptSummary flattens the surfaces used and every model observed, in first
// -seen order and without duplicates. Models stay a list because a router may
// switch mid-session: collapsing them to one would invent a fact.
func attemptSummary(attempts []Attempt) (surfaces, models []string) {
	seenSurface := map[string]bool{}
	seenModel := map[string]bool{}
	for _, a := range attempts {
		if a.Surface != "" && !seenSurface[a.Surface] {
			seenSurface[a.Surface] = true
			surfaces = append(surfaces, a.Surface)
		}
		for _, o := range a.ModelObservations {
			if !seenModel[o.Model] {
				seenModel[o.Model] = true
				models = append(models, o.Model)
			}
		}
	}
	return surfaces, models
}
