package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/learn"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/usage"
	"github.com/spf13/cobra"
)

func newLearnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "learn",
		Short: "Improve ranking from real use: promote feedback into fixtures, tune scoring weights",
		Long: `Learn closes the improve-through-use loop.

Every retrieval served over MCP lands in .neurofs/usage.jsonl; the agent's
post-task judgement (the neurofs_feedback tool) lands in .neurofs/feedback.jsonl.
Learn turns that signal into engine improvements:

  neurofs learn status    # how much signal has accumulated, active weights
  neurofs learn promote   # feedback -> audit/facts/learned-*.json fixtures
  neurofs learn tune      # optimize scoring weights against all fixtures
  neurofs learn tune --apply   # persist winners to .neurofs/weights.json

Tuning maximizes mean fact recall on the search surface, breaking ties by
fewer delivered tokens. The pivot gate stays the independent guardrail:
run 'neurofs gate' and 'neurofs bench' after applying tuned weights.`,
	}
	cmd.AddCommand(newLearnStatusCmd())
	cmd.AddCommand(newLearnPromoteCmd())
	cmd.AddCommand(newLearnTuneCmd())
	cmd.AddCommand(newLearnTuneFilesCmd())
	cmd.AddCommand(newLearnEvalCmd())
	cmd.AddCommand(newLearnFeedbackCmd())
	return cmd
}

func newLearnTuneFilesCmd() *cobra.Command {
	var (
		repoPath  string
		jsonOut   bool
		apply     bool
		passes    int
		benchPath string
		corpora   []string
	)
	cmd := &cobra.Command{
		Use:   "tune-files",
		Short: "Optimize the FILE-level ranker weights against bench question sets",
		Long: `Tune-files runs coordinate descent over the file ranker's weights
(internal/ranking — the surface that picks which files enter a bundle),
scored by top-3 precision on (question -> expected files) benchmarks.

Add cross-shape corpora so one repository cannot overfit the objective:

  neurofs learn tune-files \
    --bench /tmp/click:docs/g5_bench/click.json \
    --bench /tmp/vue:docs/g5_bench/vue.json

With --apply the winners land in .neurofs/ranking_weights.json, which
taskflow, pack, bench, and the UI load. Run 'neurofs gate' after applying.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := learnRepoPath(repoPath)
			if err != nil {
				return err
			}
			extra, err := parseBenchCorpora(corpora)
			if err != nil {
				return err
			}
			logf := func(format string, a ...any) {
				if !jsonOut {
					fmt.Printf("  "+format+"\n", a...)
				}
			}
			res, err := learn.TuneFiles(cmd.Context(), repo, learn.TuneFilesOptions{
				Passes:       passes,
				BenchPath:    benchPath,
				ExtraCorpora: extra,
				Apply:        apply,
				Logf:         logf,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			fmt.Printf("\nTune-files over %d question(s):\n", res.Questions)
			fmt.Printf("  top-3    : %.1f%% -> %.1f%%\n", res.Baseline.Top3, res.Tuned.Top3)
			fmt.Printf("  hit rate : %.0f%% -> %.0f%%\n", res.Baseline.HitRate*100, res.Tuned.HitRate*100)
			if len(res.Tuned.PerCorpus) > 0 {
				fmt.Println("  per corpus (baseline -> tuned):")
				for i, c := range res.Tuned.PerCorpus {
					baseTop3 := 0.0
					if i < len(res.Baseline.PerCorpus) {
						baseTop3 = res.Baseline.PerCorpus[i].Top3
					}
					fmt.Printf("    %-40s top-3 %.1f%% -> %.1f%% (%d questions)\n",
						c.Repo, baseTop3, c.Top3, c.Questions)
				}
			}
			if len(res.Changed) == 0 {
				fmt.Println("  weights  : no change — current weights are already the local optimum")
			} else {
				fmt.Println("  weights  :")
				for _, c := range res.Changed {
					fmt.Printf("    %-18s %.3g -> %.3g\n", c.Name, c.From, c.To)
				}
			}
			if res.Warning != "" {
				fmt.Printf("  WARN     : %s\n", res.Warning)
			}
			if res.Applied {
				fmt.Println("\nApplied to .neurofs/ranking_weights.json — run 'neurofs gate' and 'make check-retrieval' to confirm no regression.")
			} else if len(res.Changed) > 0 {
				fmt.Println("\nDry run. Re-run with --apply to persist these weights.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&apply, "apply", false, "Persist the winning weights to .neurofs/ranking_weights.json")
	cmd.Flags().IntVar(&passes, "passes", 2, "Coordinate-descent sweeps over the weight set")
	cmd.Flags().StringVar(&benchPath, "bench-file", "", "Benchmark for the primary repo (default <repo>/.neurofs-bench.json)")
	cmd.Flags().StringArrayVar(&corpora, "bench", nil,
		"Extra bench corpus as <repo>:<bench.json> (repeatable)")
	return cmd
}

// parseBenchCorpora parses --bench values of the form <repo>:<bench.json>.
func parseBenchCorpora(values []string) ([]learn.BenchCorpus, error) {
	var out []learn.BenchCorpus
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		repoPart, benchPart, ok := strings.Cut(v, ":")
		if !ok || strings.TrimSpace(benchPart) == "" {
			return nil, fmt.Errorf("bench corpus %q: want <repo>:<bench.json>", v)
		}
		repo, err := filepath.Abs(strings.TrimSpace(repoPart))
		if err != nil {
			return nil, fmt.Errorf("bench corpus %q: %w", v, err)
		}
		bench, err := filepath.Abs(strings.TrimSpace(benchPart))
		if err != nil {
			return nil, fmt.Errorf("bench corpus %q: %w", v, err)
		}
		out = append(out, learn.BenchCorpus{Repo: repo, BenchPath: bench})
	}
	return out, nil
}

func learnRepoPath(repoPath string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
		repoPath = cwd
	}
	return filepath.Abs(repoPath)
}

func newLearnStatusCmd() *cobra.Command {
	var (
		repoPath string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show accumulated usage/feedback signal and the active weights",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := learnRepoPath(repoPath)
			if err != nil {
				return err
			}
			st, err := learn.Status(repo)
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(st)
			}
			fmt.Printf("Learn signal for %s\n\n", repo)
			fmt.Printf("  usage entries    : %d\n", st.UsageCount)
			fmt.Printf("  feedback entries : %d\n", st.FeedbackCount)
			fmt.Printf("  fixtures         : %d hand-written + %d learned\n", st.HandFixtures, st.LearnedFixtures)
			if st.WeightsError != "" {
				fmt.Printf("  weights          : MALFORMED (%s) — search is falling back to defaults\n", st.WeightsError)
			} else if st.WeightsCustom {
				fmt.Printf("  weights          : tuned (%s)\n", st.WeightsPath)
				for _, c := range st.Changed {
					fmt.Printf("    %-26s %.3g (default %.3g)\n", c.Name, c.To, c.From)
				}
			} else {
				fmt.Printf("  weights          : defaults (no %s)\n", st.WeightsPath)
			}
			if st.RankingWeightsError != "" {
				fmt.Printf("  file ranker      : MALFORMED (%s) — falling back to defaults\n", st.RankingWeightsError)
			} else if st.RankingWeightsCustom {
				fmt.Printf("  file ranker      : tuned (%s)\n", st.RankingWeightsPath)
				for _, c := range st.RankingChanged {
					fmt.Printf("    %-26s %.3g (default %.3g)\n", c.Name, c.To, c.From)
				}
			} else {
				fmt.Printf("  file ranker      : defaults (no %s)\n", st.RankingWeightsPath)
			}
			if st.FeedbackCount > 0 {
				fmt.Printf("\nNext: neurofs learn promote && neurofs learn tune\n")
			} else {
				fmt.Printf("\nNo feedback yet — use the neurofs_feedback MCP tool after real tasks to accumulate signal.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Machine-readable output")
	return cmd
}

func newLearnPromoteCmd() *cobra.Command {
	var (
		repoPath string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Convert feedback into G3-style fact fixtures under audit/facts/",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := learnRepoPath(repoPath)
			if err != nil {
				return err
			}
			res, err := learn.Promote(repo)
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			for _, p := range res.Created {
				fmt.Printf("  created  %s\n", p)
			}
			fmt.Printf("Promoted %d fixture(s), %d already existed, %d feedback entries had nothing promotable.\n",
				len(res.Created), res.Existing, res.Skipped)
			if len(res.Created) > 0 {
				fmt.Println("Next: neurofs learn tune")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Machine-readable output")
	return cmd
}

func newLearnTuneCmd() *cobra.Command {
	var (
		repoPath    string
		jsonOut     bool
		apply       bool
		limit       int
		passes      int
		corpora     []string
		fixturesDir string
	)
	cmd := &cobra.Command{
		Use:   "tune",
		Short: "Optimize retrieval scoring weights against all fixtures",
		Long: `Tune runs coordinate descent over the retrieval scoring weights, evaluating
every fixture under audit/facts/*.json on the search surface with the same
fact scorer the gate's G3 uses. Objective: higher mean recall; at equal
recall, fewer delivered tokens.

A single-repo tune overfits that repo's shape (measured: an 8-fixture tune
on this repo lifted its recall 84%->94% while dropping click 67%->40%).
Add cross-shape corpora with --corpus so the objective becomes the
macro-average across repository shapes:

  neurofs learn tune --corpus /tmp/click:docs/g5_fixtures/click

Without --apply this is a dry run: it reports what would change and how much
it helps. With --apply the winning weights land in .neurofs/weights.json and
every subsequent search (MCP, CLI, gate, bench) uses them.

Each candidate evaluation runs a real search per fixture, so a tune takes
roughly (fixtures x 130) searches with the defaults — a couple of minutes on
a medium repo.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := learnRepoPath(repoPath)
			if err != nil {
				return err
			}
			extra, err := parseCorpora(corpora)
			if err != nil {
				return err
			}
			logf := func(format string, a ...any) {
				if !jsonOut {
					fmt.Printf("  "+format+"\n", a...)
				}
			}
			res, err := learn.Tune(cmd.Context(), repo, learn.TuneOptions{
				Limit:        limit,
				Passes:       passes,
				FixturesDir:  fixturesDir,
				ExtraCorpora: extra,
				Apply:        apply,
				Logf:         logf,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(res)
			}

			fmt.Printf("\nTune over %d fixture(s):\n", res.Fixtures)
			fmt.Printf("  recall : %.1f%% -> %.1f%%\n", res.Baseline.MeanRecall*100, res.Tuned.MeanRecall*100)
			fmt.Printf("  tokens : %.0f -> %.0f per fixture\n", res.Baseline.MeanTokens, res.Tuned.MeanTokens)
			if len(res.Tuned.PerCorpus) > 0 {
				fmt.Println("  per corpus (baseline -> tuned):")
				for i, c := range res.Tuned.PerCorpus {
					baseRecall, baseTokens := 0.0, 0.0
					if i < len(res.Baseline.PerCorpus) {
						baseRecall = res.Baseline.PerCorpus[i].MeanRecall
						baseTokens = res.Baseline.PerCorpus[i].MeanTokens
					}
					fmt.Printf("    %-40s recall %.1f%% -> %.1f%%, tokens %.0f -> %.0f (%d fixtures)\n",
						c.Repo, baseRecall*100, c.MeanRecall*100, baseTokens, c.MeanTokens, c.Fixtures)
				}
			}
			if len(res.Changed) == 0 {
				fmt.Println("  weights: no change — current weights are already the local optimum")
			} else {
				fmt.Println("  weights:")
				for _, c := range res.Changed {
					fmt.Printf("    %-26s %.3g -> %.3g\n", c.Name, c.From, c.To)
				}
			}
			if res.Warning != "" {
				fmt.Printf("  WARN   : %s\n", res.Warning)
			}
			if res.Applied {
				fmt.Printf("\nApplied to %s — run 'neurofs gate' and 'neurofs bench --search' to confirm no regression.\n",
					retrieval.WeightsPath(repo))
			} else if len(res.Changed) > 0 {
				fmt.Println("\nDry run. Re-run with --apply to persist these weights.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&apply, "apply", false, "Persist the winning weights to .neurofs/weights.json")
	cmd.Flags().IntVar(&limit, "limit", 8, "Search hits per fixture evaluation")
	cmd.Flags().IntVar(&passes, "passes", 2, "Coordinate-descent sweeps over the weight set")
	cmd.Flags().StringArrayVar(&corpora, "corpus", nil,
		"Extra corpus as <repo>[:<fixtures-dir>] (repeatable); default fixtures-dir is <repo>/audit/facts")
	cmd.Flags().StringVar(&fixturesDir, "fixtures-dir", "",
		"Fixtures for the primary repo (default <repo>/audit/facts)")
	return cmd
}

func newLearnEvalCmd() *cobra.Command {
	var (
		repoPath    string
		jsonOut     bool
		limit       int
		corpora     []string
		minRecall   float64
		fixturesDir string
	)
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score the active weights against all fixtures without changing anything",
		Long: `Eval runs one evaluation of the currently active weights (tuned or default)
over every fixture, reporting mean recall and delivered tokens. With
--min-recall it exits non-zero below the threshold — wire it into CI as a
retrieval regression gate, the search-surface counterpart of 'neurofs gate'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := learnRepoPath(repoPath)
			if err != nil {
				return err
			}
			extra, err := parseCorpora(corpora)
			if err != nil {
				return err
			}
			if cfg, cerr := config.New(repo); cerr == nil {
				if stale := staleIndexCount(cfg.DBPath, cfg.RepoRoot); stale > 0 {
					fmt.Fprintf(os.Stderr,
						"  WARN: index is stale — %d indexed file(s) changed on disk since the last scan; recall numbers will be distorted, run `neurofs scan` first\n",
						stale)
				}
			}
			w, custom, werr := retrieval.LoadWeights(repo)
			summary, err := learn.Evaluate(cmd.Context(), repo, fixturesDir, w, limit, extra)
			if err != nil {
				return err
			}
			if jsonOut {
				if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
					return err
				}
			} else {
				source := "defaults"
				if custom {
					source = retrieval.WeightsPath(repo)
				}
				if werr != nil {
					source = fmt.Sprintf("defaults (weights file malformed: %v)", werr)
				}
				fmt.Printf("Eval with %s:\n", source)
				fmt.Printf("  recall : %.1f%%\n", summary.MeanRecall*100)
				fmt.Printf("  tokens : %.0f per fixture\n", summary.MeanTokens)
				for _, c := range summary.PerCorpus {
					fmt.Printf("    %-40s recall %.1f%%, tokens %.0f (%d fixtures)\n",
						c.Repo, c.MeanRecall*100, c.MeanTokens, c.Fixtures)
				}
				for _, fs := range summary.PerFixture {
					if fs.Recall < 1.0 {
						fmt.Printf("  [%3.0f%%] %s\n", fs.Recall*100, fs.Question)
					}
				}
			}
			if minRecall > 0 && summary.MeanRecall < minRecall {
				return fmt.Errorf("learn eval: mean recall %.1f%% below --min-recall %.1f%%",
					summary.MeanRecall*100, minRecall*100)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Machine-readable output")
	cmd.Flags().IntVar(&limit, "limit", 8, "Search hits per fixture evaluation")
	cmd.Flags().StringArrayVar(&corpora, "corpus", nil,
		"Extra corpus as <repo>[:<fixtures-dir>] (repeatable)")
	cmd.Flags().Float64Var(&minRecall, "min-recall", 0,
		"Exit non-zero when mean recall falls below this fraction (e.g. 0.8)")
	cmd.Flags().StringVar(&fixturesDir, "fixtures-dir", "",
		"Fixtures for the primary repo (default <repo>/audit/facts)")
	return cmd
}

func newLearnFeedbackCmd() *cobra.Command {
	var (
		repoPath string
		rating   string
		query    string
		symbols  []string
		paths    []string
		missing  []string
		comment  string
	)
	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Record human feedback about a retrieval (terminal counterpart of the neurofs_feedback MCP tool)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := learnRepoPath(repoPath)
			if err != nil {
				return err
			}
			rating = strings.ToLower(strings.TrimSpace(rating))
			switch rating {
			case usage.RatingYes, usage.RatingNo, usage.RatingPartial:
			default:
				return fmt.Errorf(`--rating must be "yes", "no", or "partial"`)
			}
			entries, err := usage.Load(repo)
			if err != nil {
				return err
			}
			fb := usage.Feedback{
				Query:         strings.TrimSpace(query),
				Rating:        rating,
				UsefulSymbols: symbols,
				UsefulPaths:   paths,
				MissingFacts:  missing,
				Comment:       strings.TrimSpace(comment),
			}
			if matched, ok := usage.MatchEntry(entries, fb.Query); ok {
				fb.UsageID = matched.ID
				if fb.Query == "" {
					fb.Query = matched.Query
				}
			}
			if fb.Query == "" {
				return fmt.Errorf("no --query given and no logged retrieval to attach the feedback to")
			}
			if err := usage.AppendFeedback(repo, fb); err != nil {
				return err
			}
			fmt.Printf("Recorded %s feedback for %q (%s)\n", fb.Rating, fb.Query, usage.FeedbackPath(repo))
			fmt.Println("Next: neurofs learn promote")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	cmd.Flags().StringVar(&rating, "rating", "", `Judgement: yes, no, or partial (required)`)
	cmd.Flags().StringVar(&query, "query", "", "Query being judged (default: most recent logged retrieval)")
	cmd.Flags().StringArrayVar(&symbols, "symbol", nil, "Identifier that was actually useful (repeatable)")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "Repo-relative path that was actually useful (repeatable)")
	cmd.Flags().StringArrayVar(&missing, "missing", nil, "Identifier that should have been retrieved but wasn't (repeatable)")
	cmd.Flags().StringVar(&comment, "comment", "", "Optional one-line note")
	_ = cmd.MarkFlagRequired("rating")
	return cmd
}

// parseCorpora parses --corpus values of the form <repo>[:<fixtures-dir>].
// The fixtures dir may be relative to the cwd, matching how the flag is
// typed on the command line.
func parseCorpora(values []string) ([]learn.Corpus, error) {
	var out []learn.Corpus
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		repoPart, fixturesPart, _ := strings.Cut(v, ":")
		repo, err := filepath.Abs(strings.TrimSpace(repoPart))
		if err != nil {
			return nil, fmt.Errorf("corpus %q: %w", v, err)
		}
		c := learn.Corpus{Repo: repo}
		if strings.TrimSpace(fixturesPart) != "" {
			dir, err := filepath.Abs(strings.TrimSpace(fixturesPart))
			if err != nil {
				return nil, fmt.Errorf("corpus %q: %w", v, err)
			}
			c.FixturesDir = dir
		}
		out = append(out, c)
	}
	return out, nil
}
