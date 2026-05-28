// Command neurofs is the entry point for the NeuroFS CLI.
package main

import (
	"os"

	"github.com/neuromfs/neuromfs/internal/cli"
)

func main() {
	// Cobra already prints "Error: <msg>" to stderr on RunE failures
	// (SilenceErrors=false in root.go); printing again here produced the
	// duplicate "Error:" / "error:" pair the QA agent flagged.
	if err := cli.New().Execute(); err != nil {
		os.Exit(1)
	}
}
