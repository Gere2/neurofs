package cli

import (
	"github.com/neuromfs/neuromfs/internal/ui"
	"github.com/spf13/cobra"
)

// newProxyCmd wires the local Anthropic-compatible proxy server as a subcommand.
func newProxyCmd() *cobra.Command {
	var (
		addr string
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
			return ui.RunProxy(ui.Options{
				Addr: addr,
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "Address to bind the proxy server")
	return cmd
}
