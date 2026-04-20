// Command neurofs is the entry point for the NeuroFS CLI.
package main

import (
	"fmt"
	"os"

	"github.com/neuromfs/neuromfs/internal/cli"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
