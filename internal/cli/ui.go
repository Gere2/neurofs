package cli

import (
	"os"

	"github.com/neuromfs/neuromfs/internal/ui"
	"github.com/spf13/cobra"
)

// newUICmd wires the local UI server as a subcommand. It is deliberately
// thin — all behaviour lives in internal/ui so the CLI entrypoint stays a
// simple dispatcher.
func newUICmd() *cobra.Command {
	var (
		addr     string
		noOpen   bool
		repoPath string
		sandbox  bool
	)
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Start the local NeuroFS UI (web interface on loopback)",
		Long: `Ui launches a local HTTP server that wraps scan, pack, replay, records, and diff.
Nothing leaves loopback: the UI is just another client of the same internals.

The default address is 127.0.0.1:7777. The browser is opened automatically
unless --no-open is set (useful when running over SSH with port-forwarding).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repoPath == "" {
				if cwd, err := os.Getwd(); err == nil {
					repoPath = cwd
				}
			}
			return ui.Run(ui.Options{
				Addr:        addr,
				OpenBrowser: !noOpen,
				RepoRoot:    repoPath,
				Sandbox:     sandbox,
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "Address to bind (loopback by default)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Skip the automatic browser launch")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to pin/sandbox the UI server to (defaults to current directory)")
	cmd.Flags().BoolVar(&sandbox, "sandbox", true, "Sandbox/pin the UI server to the repository root to prevent path traversal")
	return cmd
}
