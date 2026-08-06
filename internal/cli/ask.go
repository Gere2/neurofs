package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/output"
	"github.com/Gere2/neurofs/internal/packager"
	"github.com/Gere2/neurofs/internal/ranking"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/Gere2/neurofs/internal/taskflow"
	"github.com/spf13/cobra"
)

func newAskCmd() *cobra.Command {
	var (
		budget    int
		repoPath  string
		format    string
		explain   bool
		filesOnly bool
		machine   bool
		limit     int
		minScore  float64
		jsonOut   bool
		noEmb     bool
	)

	cmd := &cobra.Command{
		Use:   "ask <query>",
		Short: "Generate a context bundle for a question",
		Long: `Ask ranks indexed files by relevance to your query, selects context
within a token budget, and prints an auditable bundle to stdout.

Each included fragment shows:
  - why it was selected (signals and weights)
  - how it is represented (full_code, signature, structural_note)
  - how many tokens it consumes

Run 'neurofs scan' first to build the index.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			query := args[0]
			if err := validateBudget(budget); err != nil {
				return fmt.Errorf("ask: %w", err)
			}

			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("ask: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("ask: config: %w", err)
			}

			// Ensure the index is fresh (auto-scans if empty or older than 24h)
			if err := taskflow.EnsureFreshIndex(cfg); err != nil {
				return fmt.Errorf("ask: auto-scan: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("ask: open index: %w", err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("ask: close index: %w", err))
				}
			}()

			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("ask: load index: %w", err)
			}

			fmt.Fprintf(os.Stderr, "NeuroFS — query: %q\n", query)
			fmt.Fprintf(os.Stderr, "  budget : %d tokens | index: %d files\n\n",
				budget, len(files))

			if !filesOnly && (limit != 0 || minScore != 0 || jsonOut || noEmb) {
				return fmt.Errorf("ask: --limit, --min-score, --json, and --no-embeddings require --files-only")
			}

			filesOpts := filesOnlyOptions{
				Limit:        limit,
				MinScore:     minScore,
				JSON:         jsonOut,
				NoEmbeddings: noEmb,
			}
			if err := validateFilesOnlyOptions(filesOpts); err != nil {
				return fmt.Errorf("ask: %w", err)
			}

			ranked := rankFilesForCLI(cmd.Context(), cfg, db, query, files, filesOpts.NoEmbeddings)

			if filesOnly {
				return writeFilesOnly(cmd.OutOrStdout(), ranked, filesOpts)
			}

			bundle, err := packager.Pack(ranked, query, packager.Options{
				Budget:           budget,
				UpgradeWithSlack: true,
				QueryTerms:       ranking.Tokenise(query),
			})
			if err != nil {
				return fmt.Errorf("ask: pack: %w", err)
			}

			includedSet := make(map[string]bool, len(bundle.Fragments))
			for _, frag := range bundle.Fragments {
				includedSet[frag.RelPath] = true
			}

			if explain {
				if err := writeExplain(cmd.ErrOrStderr(), query, ranked, includedSet); err != nil {
					return fmt.Errorf("ask: write explanation: %w", err)
				}
			} else {
				w := newReportWriter(cmd.ErrOrStderr())
				// Compact ranking summary: top 20 candidates with a ✓/space marker.
				for i, sf := range ranked {
					if sf.Score < 0.1 || i >= 20 {
						break
					}
					mark := " "
					if includedSet[sf.Record.RelPath] {
						mark = "✓"
					}
					w.printf("  [%s] %-50s score=%.2f\n",
						mark, sf.Record.RelPath, sf.Score)
				}
				if w.err != nil {
					return fmt.Errorf("ask: write ranking summary: %w", w.err)
				}
			}

			fmt.Fprintf(os.Stderr, "\n  tokens used : %d / %d (%.1f%%)\n",
				bundle.Stats.TokensUsed,
				bundle.Stats.TokensBudget,
				pctFloat(bundle.Stats.TokensUsed, bundle.Stats.TokensBudget),
			)
			fmt.Fprintf(os.Stderr, "  files       : %d included / %d considered\n",
				bundle.Stats.FilesIncluded, bundle.Stats.FilesConsidered)
			if bundle.Stats.CompressionRatio > 0 {
				fmt.Fprintf(os.Stderr, "  compression : %.1fx\n", bundle.Stats.CompressionRatio)
			}
			fmt.Fprintf(os.Stderr, "\n")

			// Write bundle to stdout.
			return output.WriteWithOptions(os.Stdout, bundle, output.Format(format), output.Options{Machine: machine})
		},
	}

	cmd.Flags().IntVar(&budget, "budget", config.DefaultBudget, "Token budget for the bundle")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&format, "format", "markdown", "Output format: markdown | json | text")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print the full scoring table (tokens, signals, per-file breakdown)")
	cmd.Flags().BoolVarP(&filesOnly, "files-only", "o", false, "Only list the ranked files and their reasons, without printing the bundle/prompt content")
	cmd.Flags().BoolVar(&machine, "machine", false, "Omit human explanations and scaffolding to save context tokens")
	cmd.Flags().IntVar(&limit, "limit", 0, "Limit files printed by --files-only (0 = all positive scores)")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "Minimum score printed by --files-only")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print --files-only results as JSON with symbols/imports metadata")
	cmd.Flags().BoolVar(&noEmb, "no-embeddings", false, "Skip embedding lookups in --files-only ranking")

	return cmd
}

func pctFloat(used, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// writeExplain prints a verbose scoring breakdown meant for humans auditing
// why a file was (or wasn't) picked. Every number the ranker produced is
// shown so the output is reproducible from code alone.
func writeExplain(dst io.Writer, query string, ranked []models.ScoredFile, included map[string]bool) error {
	w := newReportWriter(dst)
	terms := ranking.Tokenise(query)
	w.printf("NeuroFS — explain mode\n\n")
	w.printf("  query            : %q\n", query)
	if len(terms) == 0 {
		w.printf("  tokens used      : (none — query contains only stop-words or short tokens)\n")
	} else {
		w.printf("  tokens used      : [%s]\n", strings.Join(terms, ", "))
	}
	w.printf("  files considered : %d\n\n", len(ranked))

	w.printf("  signal weights:\n")
	weights := ranking.SignalWeights()
	names := make([]string, 0, len(weights))
	for k := range weights {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return weights[names[i]] > weights[names[j]] })
	for _, n := range names {
		w.printf("    %-18s %+4.1f\n", n, weights[n])
	}
	w.printf("\n")

	w.printf("  ranking breakdown (%-3s %-50s %8s %-8s):\n", "#", "file", "score", "status")
	w.printf("  %s\n", strings.Repeat("─", 80))

	for i, sf := range ranked {
		status := "dropped"
		if included[sf.Record.RelPath] {
			status = "included"
		}
		w.printf("  [%-2d] %-50s %8.2f %-8s\n",
			i+1, truncate(sf.Record.RelPath, 50), sf.Score, status)

		if len(sf.Reasons) == 0 {
			w.printf("       (no signals fired)\n")
			continue
		}
		// De-duplicate identical (signal, detail) pairs, sum their weights so
		// the sum equals the file score (modulo rounding).
		type key struct{ signal, detail string }
		agg := make(map[key]float64)
		order := make([]key, 0, len(sf.Reasons))
		for _, r := range sf.Reasons {
			k := key{r.Signal, r.Detail}
			if _, ok := agg[k]; !ok {
				order = append(order, k)
			}
			agg[k] += r.Weight
		}
		for _, k := range order {
			w.printf("       %-18s %-30s %+5.2f\n",
				k.signal, truncate(k.detail, 30), agg[k])
		}
	}
	w.printf("\n")
	return w.err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func formatReasonsSingleLine(reasons []models.InclusionReason) string {
	if len(reasons) == 0 {
		return "no signals fired"
	}
	type key struct{ signal, detail string }
	agg := make(map[key]float64)
	var order []key
	for _, r := range reasons {
		k := key{r.Signal, r.Detail}
		if _, ok := agg[k]; !ok {
			order = append(order, k)
		}
		agg[k] += r.Weight
	}
	var parts []string
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s: %s (+%.1f)", k.signal, k.detail, agg[k]))
	}
	return strings.Join(parts, ", ")
}
