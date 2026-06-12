package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/mcp"
	"github.com/spf13/cobra"
)

// newMcpCmd exposes neurofs as a Model Context Protocol server over
// stdio. Hosts (Claude Desktop, Cursor, etc.) launch this process and
// speak newline-delimited JSON-RPC 2.0 over its stdin/stdout. Stderr
// stays free for diagnostics.
func newMcpCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server over stdio (exposes 14 neurofs_* tools)",
		Long: `mcp starts a Model Context Protocol server speaking JSON-RPC 2.0
on stdin/stdout. It exposes these tools:

  neurofs_context          — broker that routes a question to outline,
                             search, excerpt, or chunk-backed bundle
  neurofs_task             — pack a prompt, optionally with agent session context
  neurofs_scan             — index a repo and return a read-only summary
  neurofs_search           — return ranked code chunks with line ranges
  neurofs_view_file        — read one repository-confined file
  neurofs_get_outline      — return repo outline or one file logic map
  neurofs_expand           — expand outline/excerpt/full via path, range, or hash
  neurofs_measure          — summarize measured context tokens by session
  neurofs_list_signatures  — return compact signatures for one file
  neurofs_get_excerpt      — return query-matching excerpts for one file
  neurofs_log_memory       — log an entry to the session ledger
  neurofs_search_memory    — search the local session memory ledger
  neurofs_export_memory    — export the session log in various formats
  neurofs_prune_memory     — prune old task session memory ledger entries

Wire it into any MCP host by configuring it as a stdio server that runs
` + "`neurofs mcp --repo /absolute/repo`" + `. Stdout is reserved for protocol traffic; logs go to stderr.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			srv := mcp.NewServer(os.Stdin, os.Stdout, os.Stderr, mcpVersion())
			repoRoot, err := mcpRepoRoot(repoPath)
			if err != nil {
				return err
			}
			// Pin all path-taking tools to one repo. Without this,
			// a malicious caller can pass {"repo": "/etc", "path": "passwd"}
			// to neurofs_view_file and read arbitrary host files (CRIT-2
			// from the security traffic agent).
			srv.SetRepoRoot(repoRoot)
			if err := srv.Run(ctx); err != nil {
				return fmt.Errorf("mcp: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to expose through MCP (defaults to current directory)")
	cmd.AddCommand(newMcpInstallCmd())
	cmd.AddCommand(newMcpUninstallCmd())
	cmd.AddCommand(newMcpDoctorCmd())
	return cmd
}

func mcpRepoRoot(repoPath string) (string, error) {
	if repoPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("mcp: cwd: %w", err)
		}
		repoPath = cwd
	}
	cfg, err := config.New(repoPath)
	if err != nil {
		return "", fmt.Errorf("mcp: config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("mcp: config: %w", err)
	}
	abs, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return "", fmt.Errorf("mcp: resolve repo: %w", err)
	}
	return abs, nil
}

func mcpVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
