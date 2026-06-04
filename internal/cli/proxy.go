package cli

import (
	"os"

	"github.com/neuromfs/neuromfs/internal/ui"
	"github.com/spf13/cobra"
)

// newProxyCmd wires the local Anthropic-compatible proxy server as a subcommand.
func newProxyCmd() *cobra.Command {
	var (
		addr     string
		repoPath string
		sandbox  bool
	)
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Start the local NeuroFS Anthropic API proxy server",
		Long: `Proxy launches a local HTTP server that acts as a transparent proxy for Anthropic's Claude API.
It automatically intercepts prompts from any developer agent (like Claude Code, Cursor, etc.),
indexes your workspace in the background, ranks files, compiles a compact context bundle,
and injects it directly into the Claude system instructions.

To use it:
  1. Start the proxy:
     neurofs proxy --addr 127.0.0.1:7777

  2. Configure your agent to use this base URL instead of Anthropic's official URL:
     export ANTHROPIC_BASE_URL=http://127.0.0.1:7777/v1
     claude`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repoPath == "" {
				if cwd, err := os.Getwd(); err == nil {
					repoPath = cwd
				}
			}
			return ui.RunProxy(ui.Options{
				Addr:     addr,
				RepoRoot: repoPath,
				Sandbox:  sandbox,
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "Address to bind the proxy server")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to pin/sandbox the proxy server to (defaults to current directory)")
	cmd.Flags().BoolVar(&sandbox, "sandbox", true, "Sandbox/pin the proxy server to the repository root to prevent path traversal")
	return cmd
}
