package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/benchmark"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
)

func newBenchCmd() *cobra.Command {
	var (
		repoPath            string
		benchArg            string
		topK                int
		minTop3             float64
		bundle              bool
		packBudget          int
		preferSignatures    bool
		maxMeanBundleTokens int
	)

	cmd := &cobra.Command{
		Use:   "bench [benchmark.json]",
		Short: "Measure top-k precision against a curated question set",
		Long: `Bench reads a list of (question, expected-files) pairs and reports
how often the ranker surfaces an expected file in the top-k results.

The default location of the benchmark file is <repo>/.neurofs-bench.json.

Use this to spot-check ranking changes before they regress real queries.
With --min-top3 you can fail the command (non-zero exit) when precision
drops below a threshold — wire this into CI as a ranking regression gate.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}

			benchPath := benchArg
			if len(args) > 0 {
				benchPath = args[0]
			}
			if benchPath == "" {
				benchPath = filepath.Join(cfg.RepoRoot, ".neurofs-bench.json")
			}

			questions, err := benchmark.LoadQuestions(benchPath)
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("bench: open index (did you run 'neurofs scan'?): %w", err)
			}
			defer db.Close()

			count, err := db.FileCount()
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("bench: index is empty — run 'neurofs scan' first")
			}

			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}

			results, summary := benchmark.Run(files, questions, benchmark.RunOptions{
				TopK:             topK,
				Project:          loadProjectInfo(db),
				ComputeBundle:    bundle || maxMeanBundleTokens > 0,
				PackBudget:       packBudget,
				PreferSignatures: preferSignatures,
			})

			fmt.Fprintf(os.Stderr, "NeuroFS — benchmark on %s\n", cfg.RepoRoot)
			fmt.Fprintf(os.Stderr, "  questions : %s\n\n", benchPath)

			var sb strings.Builder
			benchmark.FormatResults(&sb, results, summary, topK)
			fmt.Fprint(os.Stderr, sb.String())

			if minTop3 > 0 && summary.Top3 < minTop3 {
				return fmt.Errorf("bench: top-3 precision %.1f%% below threshold %.1f%%",
					summary.Top3, minTop3)
			}
			if maxMeanBundleTokens > 0 && summary.BundleMeanTokens > maxMeanBundleTokens {
				return fmt.Errorf("bench: mean bundle tokens %d exceeds ceiling %d",
					summary.BundleMeanTokens, maxMeanBundleTokens)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&benchArg, "file", "", "Path to benchmark JSON (defaults to <repo>/.neurofs-bench.json)")
	cmd.Flags().IntVar(&topK, "top-k", 3, "Rank cut-off for counting a question as a hit")
	cmd.Flags().Float64Var(&minTop3, "min-top3", 0, "Fail with non-zero exit when top-3 precision drops below this %")
	cmd.Flags().BoolVar(&bundle, "bundle", false, "Also pack a bundle per question and report mean/p50/p95 tokens")
	cmd.Flags().IntVar(&packBudget, "pack-budget", 0, "Token budget used with --bundle (default 8000)")
	cmd.Flags().BoolVar(&preferSignatures, "prefer-signatures", false, "Mirror --for claude compression when measuring bundles")
	cmd.Flags().IntVar(&maxMeanBundleTokens, "max-mean-bundle-tokens", 0, "Fail when the mean bundle token count exceeds this ceiling")
	return cmd
}
