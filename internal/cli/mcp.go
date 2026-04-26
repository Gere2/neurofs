package cli

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/neuromfs/neuromfs/internal/mcp"
	"github.com/spf13/cobra"
)

// newMcpCmd exposes neurofs as a Model Context Protocol server over
// stdio. Hosts (Claude Desktop, Cursor, etc.) launch this process and
// speak newline-delimited JSON-RPC 2.0 over its stdin/stdout. Stderr
// stays free for diagnostics.
func newMcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server over stdio (exposes neurofs_task and neurofs_scan)",
		Long: `mcp starts a Model Context Protocol server speaking JSON-RPC 2.0
on stdin/stdout. It exposes two tools:

  neurofs_task — pack a Claude-ready prompt for a query
  neurofs_scan — index a repo and return a read-only summary

Wire it into any MCP host by configuring it as a stdio server that runs
` + "`neurofs mcp`" + `. Stdout is reserved for protocol traffic; logs go to stderr.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			srv := mcp.NewServer(os.Stdin, os.Stdout, os.Stderr, mcpVersion())
			if err := srv.Run(ctx); err != nil {
				return fmt.Errorf("mcp: %w", err)
			}
			return nil
		},
	}
}

func mcpVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
