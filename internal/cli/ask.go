package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
)

func newAskCmd() *cobra.Command {
	var (
		budget   int
		repoPath string
		format   string
		explain  bool
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
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]

			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("ask: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("ask: config: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("ask: open index (did you run 'neurofs scan'?): %w", err)
			}
			defer db.Close()

			count, err := db.FileCount()
			if err != nil {
				return fmt.Errorf("ask: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("ask: index is empty — run 'neurofs scan' first")
			}

			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("ask: load index: %w", err)
			}

			fmt.Fprintf(os.Stderr, "NeuroFS — query: %q\n", query)
			fmt.Fprintf(os.Stderr, "  budget : %d tokens | index: %d files\n\n",
				budget, len(files))

			ranked := ranking.RankWithOptions(files, query, ranking.Options{
				Project: loadProjectInfo(db),
			})

			bundle, err := packager.Pack(ranked, query, packager.Options{
				Budget: budget,
				// ask is the inspection surface — slack-fill never hurts
				// readability and prevents the "10 % budget used" surprise.
				UpgradeWithSlack: true,
				// Excerpts on top-ranked TS/JS/Python files when the query
				// names symbols. ask shows representations explicitly so
				// the new "excerpt" rep is visible in the breakdown.
				QueryTerms: ranking.Tokenise(query),
			})
			if err != nil {
				return fmt.Errorf("ask: pack: %w", err)
			}

			includedSet := make(map[string]bool, len(bundle.Fragments))
			for _, frag := range bundle.Fragments {
				includedSet[frag.RelPath] = true
			}

			if explain {
				writeExplain(os.Stderr, query, ranked, includedSet)
			} else {
				// Compact ranking summary: top 20 candidates with a ✓/space marker.
				for i, sf := range ranked {
					if sf.Score < 0.1 || i >= 20 {
						break
					}
					mark := " "
					if includedSet[sf.Record.RelPath] {
						mark = "✓"
					}
					fmt.Fprintf(os.Stderr, "  [%s] %-50s score=%.2f\n",
						mark, sf.Record.RelPath, sf.Score)
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
			return output.Write(os.Stdout, bundle, output.Format(format))
		},
	}

	cmd.Flags().IntVar(&budget, "budget", config.DefaultBudget, "Token budget for the bundle")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&format, "format", "markdown", "Output format: markdown | json | text")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print the full scoring table (tokens, signals, per-file breakdown)")

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
func writeExplain(w io.Writer, query string, ranked []models.ScoredFile, included map[string]bool) {
	terms := ranking.Tokenise(query)
	fmt.Fprintf(w, "NeuroFS — explain mode\n\n")
	fmt.Fprintf(w, "  query            : %q\n", query)
	if len(terms) == 0 {
		fmt.Fprintf(w, "  tokens used      : (none — query contains only stop-words or short tokens)\n")
	} else {
		fmt.Fprintf(w, "  tokens used      : [%s]\n", strings.Join(terms, ", "))
	}
	fmt.Fprintf(w, "  files considered : %d\n\n", len(ranked))

	fmt.Fprintf(w, "  signal weights:\n")
	weights := ranking.SignalWeights()
	names := make([]string, 0, len(weights))
	for k := range weights {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return weights[names[i]] > weights[names[j]] })
	for _, n := range names {
		fmt.Fprintf(w, "    %-18s %+4.1f\n", n, weights[n])
	}
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "  ranking breakdown (%-3s %-50s %8s %-8s):\n", "#", "file", "score", "status")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("─", 80))

	for i, sf := range ranked {
		status := "dropped"
		if included[sf.Record.RelPath] {
			status = "included"
		}
		fmt.Fprintf(w, "  [%-2d] %-50s %8.2f %-8s\n",
			i+1, truncate(sf.Record.RelPath, 50), sf.Score, status)

		if len(sf.Reasons) == 0 {
			fmt.Fprintf(w, "       (no signals fired)\n")
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
			fmt.Fprintf(w, "       %-18s %-30s %+5.2f\n",
				k.signal, truncate(k.detail, 30), agg[k])
		}
	}
	fmt.Fprintf(w, "\n")
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
