package cli

import (
	"fmt"

	"github.com/Gere2/neurofs/internal/gate"
	"github.com/spf13/cobra"
)

// newG5SourceHashCmd is a build-time helper, deliberately hidden from normal
// CLI help. Make invokes an unstamped first-pass binary to calculate the exact
// source-tree digest, then stamps that digest into the final executable.
func newG5SourceHashCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:    "g5-source-hash",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			hash, err := gate.ComputeCrossShapeSourceTreeSHA256(repoPath)
			if err != nil {
				return fmt.Errorf("g5 source hash: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), hash)
			return err
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Engine source checkout (defaults to current directory)")
	return cmd
}
