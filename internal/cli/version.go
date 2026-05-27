package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is the current version of neurofs, injected at build time using ldflags.
var Version = "dev"

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of neurofs",
		Long:  `Print the version of neurofs compiled with git tag or commit hash.`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(Version)
		},
	}
	return cmd
}
