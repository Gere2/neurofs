package fsutil

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ConfineToRepo resolves raw against root and guarantees the result lives
// inside the repository. Non-existent leaves are allowed (so callers can use
// it for paths about to be created); the deepest existing prefix is followed
// through symlinks before the containment check, which matters on macOS
// where /var → /private/var would otherwise make valid paths fail asymmetric
// resolution.
//
// Returns an error on empty input or on paths that resolve outside root.
func ConfineToRepo(root, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	abs := raw
	if !filepath.IsAbs(raw) {
		abs = filepath.Join(root, raw)
	}
	abs = filepath.Clean(abs)

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}
	rootResolved := resolveExistingPrefix(rootAbs)
	absResolved := resolveExistingPrefix(abs)

	rel, err := filepath.Rel(rootResolved, absResolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must live inside the repo: %s", raw)
	}
	return absResolved, nil
}

// ConfineToRepoStrict is like ConfineToRepo but requires the target path to
// exist. Use it for read-only operations (e.g. MCP view_file) where a
// missing file is a hard error rather than an intended write target.
func ConfineToRepoStrict(root, path string) (string, error) {
	abs := path
	if !filepath.IsAbs(path) {
		abs = filepath.Join(root, path)
	}
	abs = filepath.Clean(abs)

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}

	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		rootResolved = rootAbs
	}

	absResolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("path does not exist or invalid: %w", err)
	}

	rel, err := filepath.Rel(rootResolved, absResolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must live inside the repo: %s", path)
	}
	return absResolved, nil
}

// resolveExistingPrefix canonicalises the deepest existing prefix of p
// (following symlinks) and re-attaches the remaining non-existent tail
// verbatim. Never returns an error: if nothing resolves, the input is
// returned unchanged. This keeps ConfineToRepo usable for paths that
// are about to be created.
func resolveExistingPrefix(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveExistingPrefix(parent), filepath.Base(p))
}
