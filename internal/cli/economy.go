package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neuromfs/neuromfs/internal/abeval"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/gate"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
)

// economyReport is the machine-readable payload written by --json / --out. It
// pairs the aggregate summary with every per-task row so a docs run is
// reproducible from the JSON alone.
type economyReport struct {
	Repo        string              `json:"repo"`
	SearchLimit int                 `json:"search_limit"`
	Summary     abeval.Summary      `json:"summary"`
	Tasks       []abeval.TaskResult `json:"tasks"`
}

func newEconomyCmd() *cobra.Command {
	var (
		repoPath    string
		fixturesDir string
		searchLimit int
		threshold   float64
		jsonOut     bool
		outPath     string
		gateMode    bool
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

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("economy: open index (did you run 'neurofs scan'?): %w", err)
			}
			defer db.Close()

			count, err := db.FileCount()
			if err != nil {
				return fmt.Errorf("economy: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("economy: index is empty — run 'neurofs scan' first")
			}
			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("economy: %w", err)
			}

			// Arm B: neurofs_search against the live index.
			search := func(ctx context.Context, query string, limit int) ([]abeval.SearchHit, error) {
				resp, err := retrieval.Search(ctx, retrieval.Options{
					Query: query,
					Repo:  cfg.RepoRoot,
					Limit: limit,
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

			report := economyReport{
				Repo:        cfg.RepoRoot,
				SearchLimit: searchLimit,
				Summary:     summary,
				Tasks:       results,
			}

			if outPath != "" {
				data, _ := json.MarshalIndent(report, "", "  ")
				if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
					return fmt.Errorf("economy: write %s: %w", outPath, err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "economy: wrote report to %s\n", outPath)
			}

			if jsonOut {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
					return err
				}
			} else {
				printEconomyReport(cmd.OutOrStdout(), report)
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
	return cmd
}

func printEconomyReport(w interface{ Write([]byte) (int, error) }, r economyReport) {
	p := func(format string, a ...interface{}) { fmt.Fprintf(w, format, a...) }

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
	p("  summary (%d task%s, %d scored, %d search miss):\n", s.Tasks, plural(s.Tasks), s.Scored, s.SearchMiss)
	p("    mean tokens     : neurofs_search %d | native whole %d\n", s.MeanTokensNeurofs, s.MeanTokensNative)
	p("    mean recall     : neurofs_search %.0f%% | native %.0f%% (matched)\n", s.MeanRecallNeurofs*100, s.MeanRecallNative*100)
	p("    token reduction : mean %.1f%% (median %.1f%%) at iso-recall\n", s.MeanTokenReduction*100, s.MedianTokenReduction*100)
	p("    threshold       : %.0f%%\n", s.Threshold*100)
	p("    VERDICT         : %s — %s\n", s.Verdict, s.Detail)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
