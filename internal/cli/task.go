package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/taskflow"
	"github.com/spf13/cobra"
)

// newTaskCmd returns `neurofs task <query>`, the shortest possible path
// from a repository and a question to a Claude-ready prompt. The whole
// flow (auto-scan, cache, rank, pack, write-claude) lives in
// internal/taskflow so the CLI and the web UI stay in lock-step — this
// command is now a thin presenter on top of taskflow.Run.
//
// Stdout is the prompt itself — not a path — so the command composes
// as a first-class Unix filter: `neurofs task "…" | pbcopy`,
// `neurofs task "…" > prompt.md`, `neurofs task "…" | any-llm-cli`.
// The summary (query, cache status, tokens, files, top picks, paths,
// clipboard) goes to stderr and stays out of pipes.
func newTaskCmd() *cobra.Command {
	var (
		repoPath string
		budget   int
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "task <query>",
		Short: "Prepare a Claude-ready prompt from your repo with zero decisions",
		Long: `Task is the shortest path from a repository and a question to a prompt
you can paste into Claude or Codex.

Stdout is the prompt itself — pipe it, redirect it, or let the auto-
clipboard copy handle it for you. Stderr carries a short summary (tokens,
files, top picks, cache status) so pipelines stay clean.

It auto-scans on first use and caches by (query, budget) so the same
question regenerates for free until the index moves.

Examples:
  neurofs task "why does ranking stem utility to util"
  neurofs task "implement resume-from-record" | pbcopy
  neurofs task "review my ranking changes" --budget 3000 > prompt.md
  neurofs task "resume work on seed UI" --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.TrimSpace(args[0])
			if query == "" {
				return fmt.Errorf("task: query must not be empty")
			}

			if repoPath == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("task: %w", err)
				}
				repoPath = cwd
			}

			if budget <= 0 {
				budget = config.DefaultBudget
			}

			if _, err := os.Stat(repoPath); os.IsNotExist(err) {
				// The taskflow package would hit this too, but surfacing it
				// here gives a one-line error instead of a wrapped chain.
				return fmt.Errorf("task: repo not found: %s", repoPath)
			}

			// A gentle stderr note when auto-scan is about to happen. The
			// user sees the "scanning …" banner only in the CLI path —
			// the UI handler shows a spinner instead.
			//
			// We can't detect this before calling Run without duplicating
			// the dbPath probe, so we print the banner unconditionally
			// when the index file is missing.
			cfg, err := config.New(repoPath)
			if err == nil {
				if st, statErr := os.Stat(cfg.DBPath); statErr != nil || (st != nil && st.Size() == 0) {
					fmt.Fprintf(os.Stderr, "NeuroFS — no index yet, scanning %s ...\n", cfg.RepoRoot)
				}
			}

			result, err := taskflow.Run(taskflow.Opts{
				RepoRoot: repoPath,
				Query:    query,
				Budget:   budget,
				Force:    force,
			})
			if err != nil {
				return fmt.Errorf("task: %w", err)
			}

			// Stdout: the prompt, for pipes.
			if _, err := os.Stdout.WriteString(result.Prompt); err != nil {
				return fmt.Errorf("task: write stdout: %w", err)
			}

			// Clipboard: best effort, status reported in the summary.
			clipStatus := taskflow.Clipboard([]byte(result.Prompt))

			// Stderr: the summary, for humans.
			cacheLabel := "fresh"
			if result.Reused {
				cacheLabel = "reused"
			}
			fmt.Fprintf(os.Stderr, "NeuroFS — task\n")
			fmt.Fprintf(os.Stderr, "  query     : %q\n", truncate(query, 70))
			fmt.Fprintf(os.Stderr, "  cache     : %s\n", cacheLabel)
			fmt.Fprintf(os.Stderr, "  tokens    : %d / %d\n",
				result.Stats.TokensUsed, result.Stats.TokensBudget)
			fmt.Fprintf(os.Stderr, "  files     : %d\n", result.Stats.FilesIncluded)
			if result.Stats.CompressionRatio > 0 {
				fmt.Fprintf(os.Stderr, "  ratio     : %.1fx\n", result.Stats.CompressionRatio)
			}
			for i, p := range result.TopPicks {
				fmt.Fprintf(os.Stderr, "  top[%d]    : %s (%dt, %s)\n",
					i+1, p.RelPath, p.Tokens, p.Representation)
			}
			fmt.Fprintf(os.Stderr, "  prompt    : %s\n", result.PromptPath)
			fmt.Fprintf(os.Stderr, "  bundle    : %s\n", result.BundlePath)
			fmt.Fprintf(os.Stderr, "  clipboard : %s\n", clipStatus)

			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().IntVar(&budget, "budget", config.DefaultBudget, "Token budget for the prompt")
	cmd.Flags().BoolVar(&force, "force", false, "Ignore the cache and regenerate")

	return cmd
}
