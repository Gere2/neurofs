package cli

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitChangedFiles returns the set of files git considers "changed" relative
// to the repository at repoRoot. "Changed" here means anything git-status
// reports: modified, staged, untracked, renamed. Files are returned as
// forward-slashed paths relative to repoRoot so they can be compared
// directly to models.FileRecord.RelPath.
//
// This function is intentionally best-effort: if git is missing, the
// repository is not a git worktree, or the command times out, we return an
// empty slice with nil error. The caller treats "no info" as "no boost".
func gitChangedFiles(repoRoot string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "status", "--porcelain", "--untracked-files=normal")
	var out bytes.Buffer
	cmd.Stdout = &out
	// Silence stderr — we detect failure through err and exit code.
	cmd.Stderr = &bytes.Buffer{}

	if err := cmd.Run(); err != nil {
		return nil
	}
	return parsePorcelain(out.Bytes())
}

// parsePorcelain turns `git status --porcelain` output into a list of
// repo-relative paths. The format is stable and lives in `man git-status`:
// two status columns, a space, then the path. Renames use "orig -> new"; we
// keep the new path.
func parsePorcelain(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var files []string
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		if arrow := strings.Index(path, " -> "); arrow >= 0 {
			path = path[arrow+4:]
		}
		// Strip quotes git wraps around paths with spaces.
		path = strings.Trim(path, `"`)
		path = filepath.ToSlash(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	return files
}
