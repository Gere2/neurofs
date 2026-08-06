package cli

import (
	"encoding/json"
	"fmt"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/contextusage"
	"github.com/spf13/cobra"
)

func newMeasureCmd() *cobra.Command {
	var (
		repoPath string
		session  string
		jsonOut  bool
	)

	cmd := &cobra.Command{
		Use:   "measure",
		Short: "Summarize actual context tokens used by agent sessions",
		Long: `Measure reads .neurofs/context_usage.jsonl and reports the real
context path: initial bundle tokens plus any expand calls linked by session.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("measure: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("measure: config: %w", err)
			}
			entries, err := contextusage.Read(cfg.RepoRoot, session)
			if err != nil {
				return fmt.Errorf("measure: read usage: %w", err)
			}
			summary := contextusage.Summarise(session, entries, 0)
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(summary)
			}
			w := newReportWriter(cmd.OutOrStdout())
			w.printf("NeuroFS context usage\n")
			if summary.SessionID != "" {
				w.printf("  session    : %s\n", summary.SessionID)
			}
			w.printf("  initial    : %d tokens\n", summary.InitialTokens)
			w.printf("  expansions : %d tokens (%d calls)\n", summary.ExpansionTokens, summary.Expansions)
			if summary.ExpandedFiles > 0 {
				w.printf("  files      : %d expanded", summary.ExpandedFiles)
				if summary.FullFileExpansions > 0 {
					w.printf(" (%d full-file)", summary.FullFileExpansions)
				}
				w.printf("\n")
			}
			w.printf("  total      : %d tokens\n", summary.TotalTokens)
			if summary.EstimatedSaved != 0 || summary.SavingsRatio > 0 {
				w.printf("  saved      : %d tokens vs eager full-file baseline\n", summary.EstimatedSaved)
				w.printf("  ratio      : %.2fx of baseline\n", summary.SavingsRatio)
			}
			for _, file := range summary.Files {
				w.printf("  file       : %s (%d tokens, %d calls", file.Path, file.ExpansionTokens, file.Expansions)
				if len(file.Modes) > 0 {
					w.printf(", modes=%v", file.Modes)
				}
				if len(file.Ranges) > 0 {
					w.printf(", ranges=%v", file.Ranges)
				}
				w.printf(")\n")
			}
			for _, rec := range summary.Recommendations {
				w.printf("  recommend  : %s\n", rec)
			}
			w.printf("  log        : %s\n", contextusage.Path(cfg.RepoRoot))
			if w.err != nil {
				return fmt.Errorf("measure: write report: %w", w.err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&session, "session", "", "Only summarize this context usage session")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print machine-readable JSON")
	return cmd
}
