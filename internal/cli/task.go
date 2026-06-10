package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/agentcontext"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/contextusage"
	"github.com/neuromfs/neuromfs/internal/memory"
	"github.com/neuromfs/neuromfs/internal/quality"
	"github.com/neuromfs/neuromfs/internal/storage"
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
		repoPath  string
		budget    int
		force     bool
		rate      bool
		noChunks  bool
		filesOnly bool
		machine   bool
		limit     int
		minScore  float64
		jsonOut   bool
		noEmb     bool
		agent     bool
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

			if err := validateBudget(budget); err != nil {
				return fmt.Errorf("task: %w", err)
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

			// Ensure the index is fresh (auto-scans if empty or older than 24h)
			if err := taskflow.EnsureFreshIndex(cfg); err != nil {
				return fmt.Errorf("task: auto-scan: %w", err)
			}

			if !filesOnly && (limit != 0 || minScore != 0 || jsonOut || noEmb) {
				return fmt.Errorf("task: --limit, --min-score, --json, and --no-embeddings require --files-only")
			}

			if filesOnly {
				filesOpts := filesOnlyOptions{
					Limit:        limit,
					MinScore:     minScore,
					JSON:         jsonOut,
					NoEmbeddings: noEmb,
				}
				if err := validateFilesOnlyOptions(filesOpts); err != nil {
					return fmt.Errorf("task: %w", err)
				}

				db, err := storage.Open(cfg.DBPath)
				if err != nil {
					return fmt.Errorf("task: open index: %w", err)
				}
				defer db.Close()

				files, err := db.AllFiles()
				if err != nil {
					return fmt.Errorf("task: load index: %w", err)
				}

				ranked := rankFilesForCLI(cmd.Context(), cfg, db, query, files, filesOpts.NoEmbeddings)
				return writeFilesOnly(cmd.OutOrStdout(), ranked, filesOpts)
			}

			result, err := taskflow.Run(taskflow.Opts{
				RepoRoot:      repoPath,
				Query:         query,
				Budget:        budget,
				Force:         force,
				DisableChunks: noChunks,
				Ledger:        memory.New(memory.NewSqliteStore(repoPath)),
				Machine:       machine,
			})
			if err != nil {
				return fmt.Errorf("task: %w", err)
			}

			outputPrompt := result.Prompt
			agentSession := ""
			if agent {
				agentSession = contextusage.NewSessionID(query, time.Now())
				agentPrompt, err := agentcontext.BuildPatchPrompt(repoPath, agentSession, result, agentcontext.Options{Transport: agentcontext.TransportCLI})
				if err != nil {
					return fmt.Errorf("task: agent context: %w", err)
				}
				outputPrompt = agentPrompt.Text
				_ = contextusage.Append(result.RepoRoot, contextusage.Entry{
					SessionID:      agentSession,
					Phase:          "initial_bundle",
					Command:        "task --agent",
					Query:          query,
					Tokens:         agentPrompt.InitialTokens,
					BaselineTokens: agentPrompt.BaselineTokens,
				})
			}

			// Stdout: the prompt, for pipes.
			if _, err := os.Stdout.WriteString(outputPrompt); err != nil {
				return fmt.Errorf("task: write stdout: %w", err)
			}

			// Clipboard: best effort, status reported in the summary.
			clipStatus := taskflow.Clipboard([]byte(outputPrompt))

			// Stderr: the summary, for humans.
			cacheLabel := "fresh"
			if result.Reused {
				cacheLabel = "reused"
			}
			fmt.Fprintf(os.Stderr, "NeuroFS — task\n")
			fmt.Fprintf(os.Stderr, "  query     : %q\n", truncate(query, 70))
			if result.ChunkMode {
				fmt.Fprintf(os.Stderr, "  mode      : chunks\n")
			}
			if agentSession != "" {
				fmt.Fprintf(os.Stderr, "  agent     : %s\n", agentSession)
			}
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

			if rate {
				rating, comment := promptRating(os.Stderr, os.Stdin)
				topPaths := make([]string, len(result.TopPicks))
				for i, p := range result.TopPicks {
					topPaths[i] = p.RelPath
				}
				entry := quality.Entry{
					Query:         query,
					Repo:          repoPath,
					TokensUsed:    result.Stats.TokensUsed,
					TokensBudget:  result.Stats.TokensBudget,
					FilesIncluded: result.Stats.FilesIncluded,
					TopPicks:      topPaths,
					Reused:        result.Reused,
					Rating:        rating,
					Comment:       comment,
				}
				if err := quality.Append(repoPath, entry); err != nil {
					// A failed log line is annoying, not fatal — the prompt
					// itself already went out, so we degrade to a warning.
					fmt.Fprintf(os.Stderr, "  quality   : warn: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "  quality   : logged to %s\n", quality.Path(repoPath))
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().IntVar(&budget, "budget", config.DefaultBudget, "Token budget for the prompt")
	cmd.Flags().BoolVar(&force, "force", false, "Ignore the cache and regenerate")
	cmd.Flags().BoolVar(&rate, "rate", false, "After generating, ask y/n + comment and append to .neurofs/quality.jsonl")
	cmd.Flags().BoolVar(&noChunks, "no-chunks", false, "Build the prompt from ranked whole files instead of code chunks")
	cmd.Flags().BoolVarP(&filesOnly, "files-only", "o", false, "Only list the ranked files and their reasons, without printing the prompt content")
	cmd.Flags().BoolVar(&machine, "machine", false, "Omit human explanations and scaffolding to save context tokens")
	cmd.Flags().IntVar(&limit, "limit", 0, "Limit files printed by --files-only (0 = all positive scores)")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "Minimum score printed by --files-only")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print --files-only results as JSON with symbols/imports metadata")
	cmd.Flags().BoolVar(&noEmb, "no-embeddings", false, "Skip embedding lookups in --files-only ranking")
	cmd.Flags().BoolVar(&agent, "agent", false, "Append patch context, expansion commands, and context usage measurement for coding agents")

	return cmd
}

// promptRating reads a single rating + optional comment from stdin.
// Output goes to `out` (stderr in production) so that the rating
// prompt never contaminates the stdout pipe carrying the prompt
// itself. EOF or a blank rating answer counts as skip — handy when
// the caller forgets they passed --rate inside a script.
func promptRating(out io.Writer, in io.Reader) (rating, comment string) {
	r := bufio.NewReader(in)
	fmt.Fprint(out, "  rate this prompt? [y/n/skip] (Enter to skip): ")
	line, _ := r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		rating = quality.RatingYes
	case "n", "no":
		rating = quality.RatingNo
	default:
		return quality.RatingSkip, ""
	}
	fmt.Fprint(out, "  comment (optional, one line, Enter to skip): ")
	c, _ := r.ReadString('\n')
	return rating, strings.TrimSpace(c)
}
