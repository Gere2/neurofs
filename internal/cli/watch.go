package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
)

func newWatchCmd() *cobra.Command {
	var repoPath string

	cmd := &cobra.Command{
		Use:   "watch [path]",
		Short: "Watch a repository for real-time incremental indexing",
		Long: `Watch monitors the repository for file changes, additions, and deletions,
and updates the SQLite index (.neurofs/index.db) incrementally in real-time.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				repoPath = args[0]
			}

			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("watch: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("watch: config: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("watch: %w", err)
			}
			defer db.Close()

			fmt.Fprintf(os.Stderr, "NeuroFS — running initial scan for %s...\n", cfg.RepoRoot)
			logf := func(f string, a ...any) {
				fmt.Fprintf(os.Stderr, f+"\n", a...)
			}

			// Run an initial scan to keep database synchronized before watching
			_, err = indexer.Run(cfg, db, indexer.Options{Logf: logf})
			if err != nil {
				return fmt.Errorf("watch: initial scan: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Initial scan completed. Starting real-time file watcher...\n")

			w, err := indexer.NewWatcher(cfg, db, logf)
			if err != nil {
				return fmt.Errorf("watch: %w", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if err := w.Start(ctx); err != nil {
				return fmt.Errorf("watch: %w", err)
			}
			defer w.Close()

			fmt.Fprintf(os.Stderr, "Watching for changes in %s. Press Ctrl+C to stop.\n", cfg.RepoRoot)

			// Wait for termination signal
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
			<-sigs

			fmt.Fprintf(os.Stderr, "\nStopping file watcher...\n")
			return nil
		},
	}

	return cmd
}
