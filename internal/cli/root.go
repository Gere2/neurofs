// Package cli wires up the NeuroFS command-line interface.
package cli

import (
	"github.com/spf13/cobra"
)

// New returns the root cobra command for the neurofs CLI.
func New() *cobra.Command {
	root := &cobra.Command{
		Use:   "neurofs",
		Short: "NeuroFS — a context compiler for LLMs",
		Long: `NeuroFS prepares better context for LLMs working on real repositories.

It does not try to generate code. It solves the upstream problem:
selecting, compressing, structuring, and justifying the minimum context
a model actually needs to answer a question about your codebase.`,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(newScanCmd())
	root.AddCommand(newAskCmd())
	root.AddCommand(newPackCmd())
	root.AddCommand(newTaskCmd())
	root.AddCommand(newStatsCmd())
	root.AddCommand(newBenchCmd())
	root.AddCommand(newAuditCmd())
	root.AddCommand(newUICmd())
	root.AddCommand(newMcpCmd())

	return root
}
