package cli

import (
	"fmt"
	"os"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
)

func newScanCmd() *cobra.Command {
	var (
		repoPath string
		verbose  bool
	)

	cmd := &cobra.Command{
		Use:   "scan [path]",
		Short: "Index a repository",
		Long: `Scan walks the repository, extracts symbols and imports from
source files, and persists the index in .neurofs/index.db.

Run this once before using 'ask' or 'pack'. Re-running updates the index.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				repoPath = args[0]
			}

			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			defer db.Close()

			fmt.Fprintf(os.Stderr, "NeuroFS — scanning %s\n", cfg.RepoRoot)

			logf := func(string, ...any) {}
			if verbose {
				logf = func(f string, a ...any) {
					fmt.Fprintf(os.Stderr, f+"\n", a...)
				}
			}

			stats, err := indexer.Run(cfg, db, indexer.Options{Logf: logf})
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}

			fmt.Fprintf(os.Stderr, "\n")
			fmt.Fprintf(os.Stderr, "  discovered : %d files\n", stats.Discovered)
			fmt.Fprintf(os.Stderr, "  indexed    : %d files\n", stats.Indexed)
			fmt.Fprintf(os.Stderr, "  skipped    : %d files\n", stats.Skipped)
			if stats.Removed > 0 {
				fmt.Fprintf(os.Stderr, "  removed    : %d stale records\n", stats.Removed)
			}
			if stats.Errors > 0 {
				fmt.Fprintf(os.Stderr, "  errors     : %d files\n", stats.Errors)
			}
			fmt.Fprintf(os.Stderr, "  symbols    : %d\n", stats.Symbols)
			fmt.Fprintf(os.Stderr, "  imports    : %d\n", stats.Imports)
			fmt.Fprintf(os.Stderr, "  index      : %s\n", cfg.DBPath)
			fmt.Fprintf(os.Stderr, "  time       : %s\n", stats.Duration.Round(1e6))
			fmt.Fprintf(os.Stderr, "\n")
			fmt.Fprintf(os.Stderr, "  Ready. Run: neurofs ask \"<your question>\" --budget 8000\n")

			return nil
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Print each indexed file")
	return cmd
}
