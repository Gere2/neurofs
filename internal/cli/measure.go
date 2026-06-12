package cli

import (
	"encoding/json"
	"fmt"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/contextusage"
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
			fmt.Fprintf(cmd.OutOrStdout(), "NeuroFS context usage\n")
			if summary.SessionID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  session    : %s\n", summary.SessionID)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  initial    : %d tokens\n", summary.InitialTokens)
			fmt.Fprintf(cmd.OutOrStdout(), "  expansions : %d tokens (%d calls)\n", summary.ExpansionTokens, summary.Expansions)
			if summary.ExpandedFiles > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  files      : %d expanded", summary.ExpandedFiles)
				if summary.FullFileExpansions > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), " (%d full-file)", summary.FullFileExpansions)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  total      : %d tokens\n", summary.TotalTokens)
			if summary.EstimatedSaved != 0 || summary.SavingsRatio > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  saved      : %d tokens vs eager full-file baseline\n", summary.EstimatedSaved)
				fmt.Fprintf(cmd.OutOrStdout(), "  ratio      : %.2fx of baseline\n", summary.SavingsRatio)
			}
			for _, file := range summary.Files {
				fmt.Fprintf(cmd.OutOrStdout(), "  file       : %s (%d tokens, %d calls", file.Path, file.ExpansionTokens, file.Expansions)
				if len(file.Modes) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), ", modes=%v", file.Modes)
				}
				if len(file.Ranges) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), ", ranges=%v", file.Ranges)
				}
				fmt.Fprintln(cmd.OutOrStdout(), ")")
			}
			for _, rec := range summary.Recommendations {
				fmt.Fprintf(cmd.OutOrStdout(), "  recommend  : %s\n", rec)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  log        : %s\n", contextusage.Path(cfg.RepoRoot))
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&session, "session", "", "Only summarize this context usage session")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print machine-readable JSON")
	return cmd
}
