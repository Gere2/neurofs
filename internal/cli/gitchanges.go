package cli

import "github.com/neuromfs/neuromfs/internal/fsutil"

// gitChangedFiles delegates to fsutil.GitChangedFiles so the cli package can
// call it with the same unexported name used in older code.
func gitChangedFiles(repoRoot string) []string {
	return fsutil.GitChangedFiles(repoRoot)
}
