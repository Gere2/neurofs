package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/neuromfs/neuromfs/internal/memory"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/spf13/cobra"
)

// newMemoryCmd returns the root cobra command for "neurofs memory"
func newMemoryCmd() *cobra.Command {
	var repoPath string

	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Manage the local task session ledger and portable memory",
		Long: `The memory command family provides a local-first session ledger.
It tracks developer queries, executed commands, context bundles, and outcomes
under .neurofs/ledger.jsonl, and supports context exports for external AI agents.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if repoPath == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("memory: %w", err)
				}
				repoPath = cwd
			}
			return nil
		},
	}

	cmd.PersistentFlags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")

	cmd.AddCommand(newMemoryLogCmd(&repoPath))
	cmd.AddCommand(newMemorySearchCmd(&repoPath))
	cmd.AddCommand(newMemoryExportCmd(&repoPath))
	cmd.AddCommand(newMemoryListCmd(&repoPath))

	return cmd
}

func newMemoryLogCmd(repoPath *string) *cobra.Command {
	var (
		query    string
		command  string
		outcome  string
		notes    string
		filesStr string
	)

	cmd := &cobra.Command{
		Use:   "log",
		Short: "Append a manual entry to the session ledger",
		Long:  `Logs a custom activity, query, command, or outcome to the active session.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var files []string
			if filesStr != "" {
				for _, f := range strings.Split(filesStr, ",") {
					t := strings.TrimSpace(f)
					if t != "" {
						files = append(files, t)
					}
				}
			}

			entry := models.LedgerEntry{
				Query:   strings.TrimSpace(query),
				Command: strings.TrimSpace(command),
				Outcome: strings.TrimSpace(outcome),
				Notes:   strings.TrimSpace(notes),
				Files:   files,
			}

			if entry.Query == "" && entry.Command == "" && entry.Outcome == "" && entry.Notes == "" && len(entry.Files) == 0 {
				return fmt.Errorf("at least one of --query, --command, --outcome, --notes, or --files must be set")
			}

			m := memory.New(memory.NewSqliteStore(*repoPath))
			err := m.AppendEntry(context.Background(), entry)
			if err != nil {
				return fmt.Errorf("memory log: %w", err)
			}

			sessionID, err := m.GetSessionID(context.Background())
			if err != nil {
				return fmt.Errorf("resolve session: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Successfully appended log entry to session %s.\n", sessionID)
			return nil
		},
	}

	cmd.Flags().StringVarP(&query, "query", "q", "", "The query or task details")
	cmd.Flags().StringVarP(&command, "command", "c", "", "The command executed")
	cmd.Flags().StringVarP(&outcome, "outcome", "o", "", "The outcome or result of the execution")
	cmd.Flags().StringVarP(&notes, "notes", "n", "", "Additional description or notes")
	cmd.Flags().StringVar(&filesStr, "files", "", "Comma-separated list of files associated with this action")

	return cmd
}

func newMemorySearchCmd(repoPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <term>",
		Short: "Search ledger entries matching a term",
		Long:  `Find entries in the session ledger matching the specified query, command, or note term.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			term := args[0]
			m := memory.New(memory.NewSqliteStore(*repoPath))
			results, err := m.SearchEntries(context.Background(), term)
			if err != nil {
				return fmt.Errorf("memory search: %w", err)
			}

			if len(results) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No matching entries found.\n")
				return nil
			}

			for _, e := range results {
				tStr := e.Timestamp.Format("2006-01-02 15:04:05")
				fmt.Fprintf(cmd.OutOrStdout(), "--- [%s] Session: %s ---\n", tStr, e.SessionID)
				if e.Query != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  Query:   %s\n", e.Query)
				}
				if e.BundleHash != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  Bundle:  %s\n", e.BundleHash)
				}
				if len(e.Files) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  Files:   %s\n", strings.Join(e.Files, ", "))
				}
				if e.Command != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  Command: %s\n", e.Command)
				}
				if e.Outcome != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  Outcome: %s\n", e.Outcome)
				}
				if e.Notes != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  Notes:   %s\n", e.Notes)
				}
			}

			return nil
		},
	}

	return cmd
}

func newMemoryExportCmd(repoPath *string) *cobra.Command {
	var (
		format  string
		outPath string
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a summary of the active session context",
		Long: `Export a structured, portable context summary in session_timeline (NEUROFS_SESSION.md), agents (AGENTS.md), or generic markdown formats.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format == "claude" {
				format = "session_timeline"
			}
			m := memory.New(memory.NewSqliteStore(*repoPath))
			res, err := m.ExportEntries(context.Background(), format)
			if err != nil {
				return fmt.Errorf("memory export: %w", err)
			}

			if outPath != "" {
				err = os.WriteFile(outPath, []byte(res), 0644)
				if err != nil {
					return fmt.Errorf("memory export write file: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Successfully exported session context to %s.\n", outPath)
			} else {
				fmt.Fprint(cmd.OutOrStdout(), res)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "session_timeline", "Export format: session_timeline (NEUROFS_SESSION.md), agents (AGENTS.md), or markdown")
	cmd.Flags().StringVar(&outPath, "out", "", "File path to write the output to (stdout if empty)")

	return cmd
}

func newMemoryListCmd(repoPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"show"},
		Short:   "List all ledger entries in the active session",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := memory.New(memory.NewSqliteStore(*repoPath))
			entries, err := memory.NewSqliteStore(*repoPath).Read(context.Background())
			if err != nil {
				return fmt.Errorf("read entries: %w", err)
			}

			sessionID, err := m.GetSessionID(context.Background())
			if err != nil {
				return fmt.Errorf("resolve session: %w", err)
			}

			found := false
			for _, e := range entries {
				if e.SessionID == sessionID {
					found = true
					tStr := e.Timestamp.Format("2006-01-02 15:04:05")
					fmt.Fprintf(cmd.OutOrStdout(), "--- [%s] ---\n", tStr)
					if e.Query != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  Query:   %s\n", e.Query)
					}
					if e.BundleHash != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  Bundle:  %s\n", e.BundleHash)
					}
					if len(e.Files) > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  Files:   %s\n", strings.Join(e.Files, ", "))
					}
					if e.Command != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  Command: %s\n", e.Command)
					}
					if e.Outcome != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  Outcome: %s\n", e.Outcome)
					}
					if e.Notes != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  Notes:   %s\n", e.Notes)
					}
				}
			}

			if !found {
				fmt.Fprintf(cmd.OutOrStdout(), "No entries found for active session ID: %s\n", sessionID)
			}
			return nil
		},
	}

	return cmd
}
