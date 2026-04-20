package cli

import (
	"github.com/neuromfs/neuromfs/internal/ui"
	"github.com/spf13/cobra"
)

// newUICmd wires the local UI server as a subcommand. It is deliberately
// thin — all behaviour lives in internal/ui so the CLI entrypoint stays a
// simple dispatcher.
func newUICmd() *cobra.Command {
	var (
		addr    string
		noOpen  bool
	)
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Start the local NeuroFS UI (web interface on loopback)",
		Long: `Ui launches a local HTTP server that wraps scan, pack, replay, records, and diff.
Nothing leaves loopback: the UI is just another client of the same internals.

The default address is 127.0.0.1:7777. The browser is opened automatically
unless --no-open is set (useful when running over SSH with port-forwarding).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ui.Run(ui.Options{
				Addr:        addr,
				OpenBrowser: !noOpen,
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "Address to bind (loopback by default)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Skip the automatic browser launch")
	return cmd
}
