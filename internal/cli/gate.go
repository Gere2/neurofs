package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/gate"
	"github.com/neuromfs/neuromfs/internal/taskflow"
	"github.com/spf13/cobra"
)

// newGateCmd wires the read-only pivot-readiness gate. It evaluates the
// criteria documented in docs/PIVOT_GATE.md against artefacts the local
// product already produces — quality.jsonl ratings, persisted bundles,
// and hand-written fact fixtures — and reports a per-criterion verdict
// plus an overall verdict.
//
// The CLI is intentionally thin: parsing, evaluation, aggregation, and
// rendering all live in internal/gate. This file's only job is to wire
// disk paths, invoke the bundle pipeline for fixture queries, and pick
// a process exit code from the overall verdict.
//
// Exit codes:
//
//	overall PASS / WARN / SKIP → exit 0
//	overall FAIL               → exit 1
//
// We do NOT exit non-zero on WARN. WARN is a deliberate "watch this"
// signal: a CI that wants WARNs to block can use --json and parse the
// verdict explicitly.
func newGateCmd() *cobra.Command {
	var (
		repoPath     string
		qualityPath  string
		bundlesDir   string
		fixturesDir  string
		fixtureBudg  int
		maxFixtures  int
		jsonOut      bool
		skipFixtures bool
	)

	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Evaluate pivot-readiness criteria (G1 ratings, G2 budgets, G3 fact recovery)",
		Long: `Gate reports whether the local NeuroFS product is good enough to consider
the hosted pivot. It reads three artefacts the product already produces:

  G1 — .neurofs/quality.jsonl     (yes/no ratings from 'task --rate')
  G2 — audit/bundles/*.json       (bundles persisted by 'task' and 'pack --save-bundle')
  G3 — audit/facts/*.json         (hand-written question + expects_facts fixtures)

For each fixture, gate runs taskflow.Run(force=true) against the current
index and counts which expected facts appear in the bundle content.

This command is read-only with one exception: the per-fixture taskflow
run touches .neurofs/task/ cache files. Pass --skip-fixtures to evaluate
G1 + G2 only on a fully read-only basis.

Exit code: 1 only on overall FAIL; 0 on PASS, WARN, or SKIP.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("gate: %w", err)
			}

			if qualityPath == "" {
				qualityPath = filepath.Join(cfg.RepoRoot, ".neurofs", "quality.jsonl")
			}
			if bundlesDir == "" {
				bundlesDir = filepath.Join(cfg.RepoRoot, "audit", "bundles")
			}
			if fixturesDir == "" {
				fixturesDir = filepath.Join(cfg.RepoRoot, "audit", "facts")
			}

			// G1
			entries, err := gate.LoadQualityEntries(qualityPath)
			if err != nil {
				return fmt.Errorf("gate: G1: %w", err)
			}
			g1 := gate.EvaluateG1(entries, gate.DefaultG1Thresholds())

			// G2
			snaps, err := gate.LoadBundleSnapshots(bundlesDir)
			if err != nil {
				return fmt.Errorf("gate: G2: %w", err)
			}
			g2res := gate.EvaluateG2(snaps)

			// G3 (fixture-driven). Three early exits before we hit
			// taskflow: explicit --skip-fixtures, missing index (UX
			// guard so first-run users get an actionable message
			// instead of a wrapped storage error), and the empty
			// fixture set (handled inside EvaluateG3 as SKIP).
			//
			// g3Details carries per-fixture results out of the switch
			// so the human render can show which fixture failed and
			// what facts it missed; the JSON path also includes them.
			var g3 gate.Criterion
			var g3Details []gate.FactResult
			switch {
			case skipFixtures:
				g3 = gate.Criterion{
					ID: "G3", Name: "Fact recovery", Verdict: gate.Skip,
					Detail: "skipped via --skip-fixtures",
				}
			case !indexReady(cfg.DBPath):
				// The gate is intentionally read-only against the
				// engine; it does NOT implicitly run scan. Without
				// an index, fixtures cannot be packed.
				g3 = gate.Criterion{
					ID: "G3", Name: "Fact recovery", Verdict: gate.Skip,
					Detail: "Run `neurofs scan` first to enable fact coverage fixtures.",
				}
			default:
				fixtures, err := gate.LoadFixtures(fixturesDir)
				if err != nil {
					return fmt.Errorf("gate: G3 load: %w", err)
				}
				if maxFixtures > 0 && len(fixtures) > maxFixtures {
					fixtures = fixtures[:maxFixtures]
				}
				g3Details = runFixtures(cfg.RepoRoot, fixtures, fixtureBudg)
				g3 = gate.EvaluateG3(g3Details, gate.DefaultG3Thresholds())
			}

			// G2 post-processing depends on G3 outcome.
			g2 := gate.PostprocessG2(g2res, g3)

			// G4 — drift over historical bundles. Not yet automated; we
			// emit a SKIP with a clear pointer to the manual flow.
			g4 := gate.Criterion{
				ID: "G4", Name: "Replay drift", Verdict: gate.Skip,
				Detail: "manual: `audit replay --bundle X --response Y`; automation deferred to a later iteration",
			}

			// G5 — cross-shape sanity. Manual; this command only inspects
			// the current repo.
			g5 := gate.Criterion{
				ID: "G5", Name: "Cross-shape sanity", Verdict: gate.Skip,
				Detail: "manual: re-run gate on a Go service, a TS frontend, and a Python lib",
			}

			report := gate.Report{
				Criteria:  []gate.Criterion{g1, g2, g3, g4, g5},
				G3Details: g3Details,
			}
			report.Overall = gate.Aggregate(report.Criteria)

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return fmt.Errorf("gate: encode json: %w", err)
				}
			} else {
				gate.Render(os.Stdout, report)
			}

			if report.Overall == gate.Fail {
				return fmt.Errorf("gate: overall FAIL")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&qualityPath, "quality", "", "Override path to quality.jsonl (default <repo>/.neurofs/quality.jsonl)")
	cmd.Flags().StringVar(&bundlesDir, "bundles-dir", "", "Override directory containing saved bundle JSONs (default <repo>/audit/bundles)")
	cmd.Flags().StringVar(&fixturesDir, "fixtures-dir", "", "Override directory containing G3 fact fixtures (default <repo>/audit/facts)")
	cmd.Flags().IntVar(&fixtureBudg, "fixture-budget", config.DefaultBudget, "Token budget used when re-packing fixtures for G3")
	cmd.Flags().IntVar(&maxFixtures, "max-fixtures", 0, "Cap how many fixtures to run (0 = all)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the full Report as JSON instead of the human table")
	cmd.Flags().BoolVar(&skipFixtures, "skip-fixtures", false, "Skip G3; evaluate G1 + G2 only (strictly read-only)")

	return cmd
}

// indexReady reports whether the SQLite index file exists and is
// non-empty. We check Size > 0 (not just existence) because storage.Open
// will create the file on first call elsewhere, leaving a 0-byte stub
// that would fool a plain os.Stat-only check. Mirrors the same probe
// taskflow.needsScan does, kept local so the gate command has zero
// dependency on taskflow's private helpers.
func indexReady(dbPath string) bool {
	info, err := os.Stat(dbPath)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// runFixtures invokes taskflow.Run for each fixture and scores the
// resulting bundle against the fixture's expected facts. A taskflow
// failure is captured in the FactResult.Error field instead of aborting:
// the gate is most useful when it can still report the fixtures that DID
// run, even if one is broken.
func runFixtures(repoRoot string, fixtures []gate.Fixture, budget int) []gate.FactResult {
	results := make([]gate.FactResult, 0, len(fixtures))
	for _, f := range fixtures {
		r, err := taskflow.Run(taskflow.Opts{
			RepoRoot: repoRoot,
			Query:    f.Question,
			Budget:   budget,
			Force:    true, // fresh bundle per fixture; cache hits would defeat the measurement
		})
		if err != nil {
			results = append(results, gate.FactResult{
				Fixture: f,
				Error:   err.Error(),
				// recall stays 0; counted in the mean as a hard miss.
			})
			continue
		}
		fr := gate.ScoreBundleAgainstFacts(r.Bundle, f.ExpectsFacts)
		fr.Fixture = f
		results = append(results, fr)
	}
	return results
}
