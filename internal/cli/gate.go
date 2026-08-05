package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/atomicfile"
	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/gate"
	"github.com/Gere2/neurofs/internal/grounding"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/Gere2/neurofs/internal/taskflow"
	"github.com/spf13/cobra"
)

// newGateCmd wires the read-only pivot-readiness gate. It evaluates the
// criteria documented in docs/PIVOT_GATE.md against artefacts the local
// product already produces — quality ratings, persisted bundles, fact
// fixtures, drift observations, and cross-shape evidence — and reports a
// per-criterion verdict plus an overall verdict.
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
		repoPath      string
		qualityPath   string
		bundlesDir    string
		fixturesDir   string
		baselinePath  string
		g5EvidenceDir string
		fixtureBudg   int
		maxFixtures   int
		jsonOut       bool
		skipFixtures  bool
		noChunks      bool
		g5Attest      bool
		g5EngineRoot  string
		outPath       string
	)

	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Evaluate all pivot-readiness criteria (G1-G5)",
		Long: `Gate reports whether the local NeuroFS product is good enough to consider
the hosted pivot. It reads the audit artefacts the product already produces:

  G1 — .neurofs/quality.jsonl     (yes/no ratings from 'task --rate')
  G2 — audit/bundles/*.json       (bundles persisted by 'task' and 'pack --save-bundle')
  G3 — audit/facts/*.json         (hand-written question + expects_facts fixtures)
  G4 — audit records/pairs/ledger (replay drift observations)
  G5 — audit/g5/*.json            (immutable mechanical cross-shape runs)

For each fixture, gate runs taskflow.Run(force=true) against the current
index and counts which expected facts appear in the bundle content.

This command is read-only with one exception: the per-fixture taskflow
run touches .neurofs/task/ cache files. Pass --skip-fixtures to skip G3
and keep the remaining G1/G2/G4/G5 checks fully read-only.

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
			if g5EvidenceDir == "" {
				g5EvidenceDir = filepath.Join(cfg.RepoRoot, "audit", "g5")
			}
			if g5Attest &&
				((!jsonOut && outPath == "") ||
					fixtureBudg != config.DefaultBudget ||
					maxFixtures != 0 ||
					skipFixtures ||
					noChunks) {
				return fmt.Errorf(
					"gate: --g5-attest requires retained JSON via --json or --out, fixture budget %d, chunks enabled, and the full fixture set",
					config.DefaultBudget,
				)
			}
			if g5Attest && g5EngineRoot == "" {
				return fmt.Errorf("gate: --g5-attest requires --g5-engine-root")
			}
			if g5Attest && cmd.Flags().Changed("bundles-dir") {
				return fmt.Errorf(
					"gate: --g5-attest derives G2 from the fresh G3 fixture bundles and does not accept --bundles-dir",
				)
			}

			// G1
			entries, err := gate.LoadQualityEntries(qualityPath)
			if err != nil {
				return fmt.Errorf("gate: G1: %w", err)
			}
			g1 := gate.EvaluateG1(entries, gate.DefaultG1Thresholds())

			// Normal gate runs evaluate persisted G2 history. A G5
			// attestation replaces this below with exactly one fresh bundle
			// from each G3 fixture, binding both criteria to one invocation.
			var snaps []gate.BundleSnapshot
			if !g5Attest {
				snaps, err = gate.LoadBundleSnapshots(bundlesDir)
				if err != nil {
					return fmt.Errorf("gate: G2: %w", err)
				}
			}

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
			var fixtureBundleAttestations []gate.CrossShapeFixtureBundleAttestation
			indexIssue := ""
			if !skipFixtures && indexReady(cfg.DBPath) {
				indexIssue = gateIndexFreshnessIssue(cfg)
			}
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
			case indexIssue != "":
				g3 = gate.Criterion{
					ID: "G3", Name: "Fact recovery", Verdict: gate.Skip,
					Detail: "index is not a fresh read-only measurement: " + indexIssue + "; run `neurofs scan` first",
				}
			default:
				fixtures, err := gate.LoadFixtures(fixturesDir)
				if err != nil {
					return fmt.Errorf("gate: G3 load: %w", err)
				}
				if maxFixtures > 0 && len(fixtures) > maxFixtures {
					fixtures = fixtures[:maxFixtures]
				}
				var freshSnapshots []gate.BundleSnapshot
				g3Details, freshSnapshots, fixtureBundleAttestations = runFixtures(
					cmd.Context(),
					cfg.RepoRoot,
					fixtures,
					fixtureBudg,
					noChunks,
				)
				if g5Attest {
					snaps = freshSnapshots
				}
				gate.MarkStaleFacts(cfg.RepoRoot, g3Details)
				g3 = gate.EvaluateG3(g3Details, gate.DefaultG3Thresholds())
			}

			// G2 post-processing depends on G3 outcome.
			g2res := gate.EvaluateG2(snaps)
			g2 := gate.PostprocessG2(g2res, g3)

			// G4 — replay drift, pooled from every available source:
			// persisted records, stem-paired bundle+response files
			// (recomputed against the bundle bytes on disk), and
			// response-kind events from the continuous grounding ledger.
			recordsDir := filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir)
			paths, err := audit.ListRecords(recordsDir)
			if err != nil {
				return fmt.Errorf("gate: G4 records: %w", err)
			}
			var records []audit.AuditRecord
			for _, p := range paths {
				rec, err := audit.LoadRecord(p)
				if err != nil {
					return fmt.Errorf("gate: G4 record %s: %w", p, err)
				}
				records = append(records, rec)
			}
			samples := gate.SamplesFromRecords(records)
			pairSamples, err := gate.CollectPairDrift(bundlesDir, filepath.Join(cfg.RepoRoot, "audit", "responses"))
			if err != nil {
				return fmt.Errorf("gate: G4 pairs: %w", err)
			}
			samples = append(samples, pairSamples...)
			events, err := grounding.Read(cfg.RepoRoot)
			if err != nil {
				return fmt.Errorf("gate: G4 grounding ledger: %w", err)
			}
			for _, ev := range events {
				if ev.Kind != grounding.KindResponse {
					continue // edit-kind drift can be legitimate new code
				}
				samples = append(samples, gate.DriftSample{
					Origin: "grounding",
					Label:  ev.SessionID,
					Rate:   ev.DriftRate,
				})
			}
			g4 := gate.EvaluateG4Samples(samples, gate.DefaultG4Thresholds())

			// G5 — immutable mechanical evidence from the three required
			// repository shapes. Agent-produced runs are valid here because
			// the measurements are deterministic; only G1 requires a human.
			crossShapeEvidence, err := gate.LoadCrossShapeEvidence(g5EvidenceDir)
			if err != nil {
				return fmt.Errorf("gate: G5: %w", err)
			}
			g5 := gate.EvaluateG5ForRepo(crossShapeEvidence, cfg.RepoRoot)

			var metadata *gate.CrossShapeReportMetadata
			if g5Attest {
				metadata, err = gate.BuildCrossShapeReportMetadata(
					cfg.RepoRoot,
					fixturesDir,
					g5EngineRoot,
					cfg.HybridMode,
				)
				if err != nil {
					return fmt.Errorf("gate: G5 attestation: %w", err)
				}
			}
			report := gate.Report{
				Criteria:   []gate.Criterion{g1, g2, g3, g4, g5},
				G3Details:  g3Details,
				G5Metadata: metadata,
			}
			if g5Attest {
				report.G5GateInvocation = &gate.CrossShapeGateInvocation{
					FixtureBudget:  fixtureBudg,
					NoChunks:       noChunks,
					MaxFixtures:    maxFixtures,
					SkipFixtures:   skipFixtures,
					FixtureBundles: fixtureBundleAttestations,
				}
			}
			report.Overall = gate.Aggregate(report.Criteria)

			// --baseline compares the current report against a prior
			// `gate --json` output. Regressions block the PR even when
			// the absolute thresholds still pass — main may already be
			// at recall=0.85 against a 0.80 floor; a PR that takes it
			// to 0.82 is still a regression worth blocking.
			if baselinePath != "" {
				baseline, err := gate.LoadBaseline(baselinePath)
				if err != nil {
					return fmt.Errorf("gate: %w", err)
				}
				report.Regressions = gate.Diff(report, baseline)
			}

			jsonReport, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return fmt.Errorf("gate: encode json: %w", err)
			}
			jsonReport = append(jsonReport, '\n')
			if outPath != "" {
				if err := atomicfile.WriteFile(outPath, jsonReport, 0o644); err != nil {
					return fmt.Errorf("gate: write %s: %w", outPath, err)
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "gate: wrote report to %s\n", outPath); err != nil {
					return fmt.Errorf("gate: write report path: %w", err)
				}
			}
			if jsonOut {
				if _, err := cmd.OutOrStdout().Write(jsonReport); err != nil {
					return fmt.Errorf("gate: write json: %w", err)
				}
			} else {
				if err := gate.Render(cmd.OutOrStdout(), report); err != nil {
					return fmt.Errorf("gate: render report: %w", err)
				}
			}

			if report.Overall == gate.Fail {
				return fmt.Errorf("gate: overall FAIL")
			}
			if len(report.Regressions) > 0 {
				return fmt.Errorf("gate: %d regression(s) vs baseline", len(report.Regressions))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&qualityPath, "quality", "", "Override path to quality.jsonl (default <repo>/.neurofs/quality.jsonl)")
	cmd.Flags().StringVar(&bundlesDir, "bundles-dir", "", "Override directory containing saved bundle JSONs (default <repo>/audit/bundles)")
	cmd.Flags().StringVar(&fixturesDir, "fixtures-dir", "", "Override directory containing G3 fact fixtures (default <repo>/audit/facts)")
	cmd.Flags().StringVar(&g5EvidenceDir, "g5-evidence-dir", "", "Override directory containing immutable G5 cross-shape runs (default <repo>/audit/g5)")
	cmd.Flags().IntVar(&fixtureBudg, "fixture-budget", config.DefaultBudget, "Token budget used when re-packing fixtures for G3")
	cmd.Flags().IntVar(&maxFixtures, "max-fixtures", 0, "Cap how many fixtures to run (0 = all)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the full Report as JSON instead of the human table")
	cmd.Flags().BoolVar(&skipFixtures, "skip-fixtures", false, "Skip G3; evaluate G1/G2/G4/G5 without touching fixture task caches")
	cmd.Flags().BoolVar(&noChunks, "no-chunks", false, "Disable chunk-based packing during fixture evaluation")
	cmd.Flags().BoolVar(&g5Attest, "g5-attest", false, "Attach runtime identity and canonical G3 flags to a retained G5 JSON report")
	cmd.Flags().StringVar(&g5EngineRoot, "g5-engine-root", "", "NeuroFS source checkout used to build the attested binary (required with --g5-attest)")
	cmd.Flags().StringVar(&outPath, "out", "", "Also write the full JSON report atomically to this path")
	cmd.Flags().StringVar(&baselinePath, "baseline", "", "Path to a prior `gate --json` output; report and fail on regressions vs baseline")

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

// gateIndexFreshnessIssue validates every generation boundary without writing:
// schema/indexer version, embedding space, and the complete source set
// (including added/deleted files). G3 must not grade an incomplete snapshot.
func gateIndexFreshnessIssue(cfg *config.Config) string {
	db, err := storage.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = db.Close() }()

	stale, err := indexer.RequiresReindex(db)
	if err != nil {
		return err.Error()
	}
	if stale {
		return "indexer version changed"
	}
	client := embeddings.NewClient(cfg.HybridMode)
	if err := client.Validate(); err != nil {
		return "embedding configuration: " + err.Error()
	}
	stale, err = indexer.RequiresEmbeddingReindex(db, client.ProviderName(), client.ModelName())
	if err != nil {
		return err.Error()
	}
	if stale {
		return "embedding provider or model changed"
	}
	stale, err = indexer.RequiresSourceReindex(cfg, db)
	if err != nil {
		return err.Error()
	}
	if stale {
		return "indexed source generation changed"
	}
	return ""
}

// staleIndexCount opens the index read-only and counts files whose disk
// content no longer matches their indexed checksum. Errors degrade to 0:
// the staleness warning is a courtesy, never a gate blocker.
func staleIndexCount(dbPath, repoRoot string) int {
	db, err := storage.OpenReadOnly(dbPath)
	if err != nil {
		return 0
	}
	// This helper is best-effort by contract: every storage failure degrades
	// to "no warning", including a close failure after the read.
	defer func() { _ = db.Close() }()
	files, err := db.AllFiles()
	if err != nil {
		return 0
	}
	return gate.CountStaleIndexFiles(repoRoot, files)
}

// runFixtures invokes taskflow.Run for each fixture and scores the
// resulting bundle against the fixture's expected facts. A taskflow
// failure is captured in the FactResult.Error field instead of aborting:
// the gate is most useful when it can still report the fixtures that DID
// run, even if one is broken.
func runFixtures(
	ctx context.Context,
	repoRoot string,
	fixtures []gate.Fixture,
	budget int,
	noChunks bool,
) (
	[]gate.FactResult,
	[]gate.BundleSnapshot,
	[]gate.CrossShapeFixtureBundleAttestation,
) {
	results := make([]gate.FactResult, 0, len(fixtures))
	snapshots := make([]gate.BundleSnapshot, 0, len(fixtures))
	attestations := make([]gate.CrossShapeFixtureBundleAttestation, 0, len(fixtures))
	for _, f := range fixtures {
		r, err := taskflow.Run(taskflow.Opts{
			Context:       ctx,
			RepoRoot:      repoRoot,
			Query:         f.Question,
			Budget:        budget,
			Force:         true, // fresh bundle per fixture; cache hits would defeat the measurement
			DisableChunks: noChunks,
			// Gate measures the already-scanned generation. Age, indexer
			// version, embedding configuration, and source drift must never
			// rewrite the evidence under measurement.
			DisableIndexRefresh: true,
			Ledger:              nil, // Explicitly disable session ledger side-effects
		})
		if err != nil {
			results = append(results, gate.FactResult{
				Fixture: f,
				Error:   err.Error(),
				// recall stays 0; counted in the mean as a hard miss.
			})
			continue
		}
		if r.Bundle.BundleHash == "" ||
			r.Bundle.HashAlgorithm != audit.BundleHashAlgorithm ||
			r.Bundle.BundleHash != audit.BundleHash(r.Bundle) {
			results = append(results, gate.FactResult{
				Fixture: f,
				Error:   "generated bundle has an invalid canonical hash",
			})
			continue
		}
		fr := gate.ScoreBundleAgainstFacts(r.Bundle, f.ExpectsFacts)
		fr.Fixture = f
		results = append(results, fr)
		snapshots = append(snapshots, gate.BundleSnapshot{
			Path:   r.BundlePath,
			Used:   r.Bundle.Stats.TokensUsed,
			Budget: r.Bundle.Stats.TokensBudget,
		})
		questionHash := sha256.Sum256([]byte(f.Question))
		attestations = append(attestations, gate.CrossShapeFixtureBundleAttestation{
			FixtureSource:  filepath.Base(f.SourcePath),
			QuestionSHA256: fmt.Sprintf("%x", questionHash),
			BundleHash:     r.Bundle.BundleHash,
			TokensUsed:     r.Bundle.Stats.TokensUsed,
			TokensBudget:   r.Bundle.Stats.TokensBudget,
		})
	}
	return results, snapshots, attestations
}
