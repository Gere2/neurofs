package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/abeval"
	"github.com/Gere2/neurofs/internal/atomicfile"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/gate"
	"github.com/Gere2/neurofs/internal/retrieval"
	"github.com/spf13/cobra"
)

// economyReport is the machine-readable payload written by --json / --out. It
// pairs the aggregate summary with every per-task row so a docs run is
// reproducible from the JSON alone.
type economyReport struct {
	Repo        string                         `json:"repo"`
	SearchLimit int                            `json:"search_limit"`
	Summary     abeval.Summary                 `json:"summary"`
	Tasks       []abeval.TaskResult            `json:"tasks"`
	G5Metadata  *gate.CrossShapeReportMetadata `json:"g5_metadata,omitempty"`
}

func newEconomyCmd() *cobra.Command {
	var (
		repoPath     string
		fixturesDir  string
		searchLimit  int
		threshold    float64
		jsonOut      bool
		outPath      string
		gateMode     bool
		g5Attest     bool
		g5EngineRoot string
	)

	cmd := &cobra.Command{
		Use:   "economy",
		Short: "Phase-0 A/B: iso-recall token cost, neurofs_search vs native whole files",
		Long: `Economy runs a reproducible, iso-recall A/B comparison of how many context
tokens it costs to ground a set of tasks two ways:

  A (baseline) — native retrieval: read whole files until the answer is in hand.
  B (NeuroFS)  — neurofs_search: targeted, citable excerpts (line ranges).

For each task arm B runs neurofs_search and we record its snippet tokens and
fact recall (the same audit.ScoreFacts scorer the gate uses). Arm A then reads
the whole files B's hits came from, in hit order, accumulating only until its
recall reaches B's. The two arms are therefore compared at the SAME recall and
the headline metric is the mean token reduction (1 - tokensB/tokensA).

This baseline is conservative: native reads exactly the files NeuroFS surfaced
and stops the moment it matches NeuroFS's recall, so the measured savings are a
lower bound on the advantage over a naive agent that opens more files.

Tasks default to the G3 fact fixtures in <repo>/audit/facts.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("economy: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("economy: config: %w", err)
			}
			if fixturesDir == "" {
				fixturesDir = filepath.Join(cfg.RepoRoot, "audit", "facts")
			}
			if g5Attest {
				switch {
				case searchLimit != abeval.DefaultSearchLimit ||
					threshold != abeval.DefaultThreshold:
					return fmt.Errorf(
						"economy: --g5-attest requires --search-limit %d and --threshold %.2f",
						abeval.DefaultSearchLimit, abeval.DefaultThreshold,
					)
				case !jsonOut && outPath == "":
					return fmt.Errorf("economy: --g5-attest requires retained JSON via --json or --out")
				case g5EngineRoot == "":
					return fmt.Errorf("economy: --g5-attest requires --g5-engine-root")
				}
			}

			fixtures, err := gate.LoadFixtures(fixturesDir)
			if err != nil {
				return fmt.Errorf("economy: load tasks: %w", err)
			}
			if len(fixtures) == 0 {
				return fmt.Errorf("economy: no fact fixtures in %s — write some or pass --fixtures-dir", fixturesDir)
			}
			tasks := make([]abeval.Task, 0, len(fixtures))
			for _, f := range fixtures {
				tasks = append(tasks, abeval.Task{
					Question:     f.Question,
					ExpectsFacts: f.ExpectsFacts,
					Source:       filepath.Base(f.SourcePath),
				})
			}

			// Refresh once before capturing any records, then keep every arm
			// on the returned immutable in-memory generation. Previously the
			// first search could reindex after AllFiles had already supplied
			// the native baseline, mixing new snippets with stale checksums.
			session, err := retrieval.NewSession(cmd.Context(), cfg.RepoRoot)
			if err != nil {
				return fmt.Errorf("economy: open index (did you run 'neurofs scan'?): %w", err)
			}
			files := session.SnapshotFiles()
			if len(files) == 0 {
				return fmt.Errorf("economy: index is empty — run 'neurofs scan' first")
			}

			// Arm B searches the exact session generation that supplied the
			// native file records above. DisableIndexRefresh documents and
			// preserves the measurement boundary for this call path.
			search := func(ctx context.Context, query string, limit int) ([]abeval.SearchHit, error) {
				resp, err := session.Search(ctx, retrieval.Options{
					Query:                   query,
					Repo:                    cfg.RepoRoot,
					Limit:                   limit,
					DisableIndexRefresh:     true,
					ExpandStructuralContext: true,
				})
				if err != nil {
					return nil, err
				}
				hits := make([]abeval.SearchHit, 0, len(resp.Results))
				for _, h := range resp.Results {
					hits = append(hits, abeval.SearchHit{
						Path:    h.Path,
						Snippet: h.Snippet,
						Tokens:  h.TokenEstimate,
					})
				}
				return hits, nil
			}

			results, summary, err := abeval.Run(cmd.Context(), files, tasks, search, abeval.Options{
				SearchLimit: searchLimit,
				Threshold:   threshold,
			})
			if err != nil {
				return fmt.Errorf("economy: %w", err)
			}

			var metadata *gate.CrossShapeReportMetadata
			if g5Attest {
				metadata, err = gate.BuildCrossShapeReportMetadata(
					cfg.RepoRoot,
					fixturesDir,
					g5EngineRoot,
					cfg.HybridMode,
				)
				if err != nil {
					return fmt.Errorf("economy: G5 attestation: %w", err)
				}
			}
			report := economyReport{
				Repo:        cfg.RepoRoot,
				SearchLimit: searchLimit,
				Summary:     summary,
				Tasks:       results,
				G5Metadata:  metadata,
			}

			if outPath != "" {
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("economy: encode report: %w", err)
				}
				if err := atomicfile.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
					return fmt.Errorf("economy: write %s: %w", outPath, err)
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "economy: wrote report to %s\n", outPath); err != nil {
					return fmt.Errorf("economy: write report path: %w", err)
				}
			}

			if jsonOut {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
					return err
				}
			} else {
				if err := printEconomyReport(cmd.OutOrStdout(), report); err != nil {
					return fmt.Errorf("economy: write report: %w", err)
				}
			}

			if gateMode && summary.Verdict == "FAIL" {
				return fmt.Errorf("economy: gate FAIL — %s", summary.Detail)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&fixturesDir, "fixtures-dir", "", "Directory of question+expects_facts fixtures (default <repo>/audit/facts)")
	cmd.Flags().IntVar(&searchLimit, "search-limit", abeval.DefaultSearchLimit, "neurofs_search hits to keep per task (arm B)")
	cmd.Flags().Float64Var(&threshold, "threshold", abeval.DefaultThreshold, "Minimum mean iso-recall token reduction for a PASS verdict")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print machine-readable JSON")
	cmd.Flags().StringVar(&outPath, "out", "", "Also write the full JSON report to this path")
	cmd.Flags().BoolVar(&gateMode, "gate", false, "Exit non-zero when the verdict is FAIL")
	cmd.Flags().BoolVar(&g5Attest, "g5-attest", false, "Attach runtime identity for a canonical retained G5 report")
	cmd.Flags().StringVar(&g5EngineRoot, "g5-engine-root", "", "NeuroFS source checkout used to build the attested binary (required with --g5-attest)")
	return cmd
}

func printEconomyReport(dst interface{ Write([]byte) (int, error) }, r economyReport) error {
	w := newReportWriter(dst)
	p := w.printf

	p("NeuroFS — Phase-0 economy A/B (iso-recall) on %s\n", r.Repo)
	p("  arm B = neurofs_search (limit %d); arm A = native whole files to match B's recall\n\n", r.SearchLimit)

	for _, t := range r.Tasks {
		mark := " "
		if t.Scored {
			if t.TokenReduction >= r.Summary.Threshold {
				mark = "✓"
			} else {
				mark = "·"
			}
		}
		p("  [%s] %q\n", mark, truncateLine(t.Question, 62))
		if !t.Scored {
			p("       (not scored — %s)\n\n", t.Note)
			continue
		}
		p("       neurofs_search : %5d tok  recall %3.0f%%  (%d file%s)\n",
			t.Neurofs.Tokens, t.Neurofs.Recall*100, len(t.Neurofs.Files), plural(len(t.Neurofs.Files)))
		p("       native whole   : %5d tok  recall %3.0f%%  (%d file%s, iso-recall)\n",
			t.NativeIso.Tokens, t.NativeIso.Recall*100, len(t.NativeIso.Files), plural(len(t.NativeIso.Files)))
		p("       reduction      : %.1f%% fewer tokens at equal recall\n\n", t.TokenReduction*100)
	}

	s := r.Summary
	p("  summary (%d task%s, %d fact, %d scored, %d search miss):\n", s.Tasks, plural(s.Tasks), s.FactTasks, s.Scored, s.SearchMiss)
	p("    overall recall  : neurofs_search %.0f%% over %d fact task%s (misses count as 0)\n",
		s.OverallRecallNeurofs*100, s.FactTasks, plural(s.FactTasks))
	if s.MissRate > 0 {
		p("    miss rate       : %.0f%% (savings below cover only the answerable subset)\n", s.MissRate*100)
	}
	p("    mean tokens     : neurofs_search %d | native whole %d  (scored subset)\n", s.MeanTokensNeurofs, s.MeanTokensNative)
	p("    iso recall      : neurofs_search %.0f%% | native %.0f%% (matched, scored subset)\n", s.MeanRecallNeurofs*100, s.MeanRecallNative*100)
	p("    token reduction : mean %.1f%% (median %.1f%%) at iso-recall\n", s.MeanTokenReduction*100, s.MedianTokenReduction*100)
	p("    threshold       : %.0f%%\n", s.Threshold*100)
	p("    VERDICT         : %s — %s\n", s.Verdict, s.Detail)
	return w.err
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
