// Package fsutil provides file-system helpers for NeuroFS.
package fsutil

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// ignoredDirsAnywhere contains directory names that should be skipped
// at any depth in the tree. These are tooling caches, build artefacts,
// and convention-driven non-source dirs whose meaning does not depend
// on where they sit in the repo.
var ignoredDirsAnywhere = map[string]bool{
	".git":          true,
	".neurofs":      true,
	".claude":       true, // Claude Code metadata + ephemeral worktrees under .claude/worktrees/**
	"testdata":      true, // Go convention: testdata/ is fixtures, not the project's own code — ignored by `go build` and should be by ranking too
	"fixtures":      true,
	"node_modules":  true,
	"vendor":        true,
	"dist":          true,
	"build":         true,
	".next":         true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	".mypy_cache":   true,
	".pytest_cache": true,
	"coverage":      true,
	".coverage":     true,
	"target":        true, // Rust / Java
	"out":           true,
	".idea":         true,
	".vscode":       true,
}

// ignoredDirsTopOnly carries directory names that should be skipped ONLY
// when they sit directly under the repo root. The ranking traffic agent
// surfaced the original bug: a blanket basename match on "audit" also
// hid internal/audit/ (the package implementing audit replay,
// BundleHash, and the governance pitch). NeuroFS was blind to its own
// production code about audit, so any query about "where is bundle_hash
// computed" returned subpar results.
var ignoredDirsTopOnly = map[string]bool{
	"audit":  true, // NeuroFS's own audit/{bundles,records,responses,notes} — re-ingesting our outputs poisons ranking
	".audit": true,
}

// ignoredExts contains file extensions that carry no useful code context.
var ignoredExts = map[string]bool{
	".png":    true,
	".jpg":    true,
	".jpeg":   true,
	".gif":    true,
	".svg":    true,
	".ico":    true,
	".woff":   true,
	".woff2":  true,
	".ttf":    true,
	".eot":    true,
	".mp4":    true,
	".mp3":    true,
	".zip":    true,
	".tar":    true,
	".gz":     true,
	".lock":   true,
	".sum":    true,
	".pdf":    true,
	".bin":    true,
	".exe":    true,
	".dll":    true,
	".so":     true,
	".dylib":  true,
	".map":    true, // source maps
	".min.js": true,
}

// extToLang maps file extensions to a Lang value.
var extToLang = map[string]models.Lang{
	".ts":    models.LangTypeScript,
	".tsx":   models.LangTypeScript,
	".mts":   models.LangTypeScript,
	".js":    models.LangJavaScript,
	".jsx":   models.LangJavaScript,
	".mjs":   models.LangJavaScript,
	".cjs":   models.LangJavaScript,
	".py":    models.LangPython,
	".go":    models.LangGo,
	".md":    models.LangMarkdown,
	".mdx":   models.LangMarkdown,
	".json":  models.LangJSON,
	".yaml":  models.LangYAML,
	".yml":   models.LangYAML,
	".rs":    models.LangRust,
	".cpp":   models.LangCpp,
	".hpp":   models.LangCpp,
	".cc":    models.LangCpp,
	".h":     models.LangCpp,
	".java":  models.LangJava,
	".rb":    models.LangRuby,
}

// LangForPath returns the language for the given file path.
func LangForPath(path string) models.Lang {
	base := strings.ToLower(filepath.Base(path))

	// Handle compound extensions like .min.js before single-ext lookup.
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") {
		return models.LangUnknown
	}

	ext := strings.ToLower(filepath.Ext(base))
	if lang, ok := extToLang[ext]; ok {
		return lang
	}
	return models.LangUnknown
}

// IsSupported returns true if the file has a language NeuroFS can index.
func IsSupported(path string) bool {
	return LangForPath(path) != models.LangUnknown
}

// ShouldSkipDir returns true if the directory basename is in either
// ignore set. Kept exported because callers without repo-root context
// rely on it (legacy tests, dir-name surveys). Prefer ShouldSkipDirAt
// for any walk that knows the repo root — the basename-only version
// will mistakenly skip nested directories like internal/audit/ that
// only deserve to be ignored at top level.
func ShouldSkipDir(name string) bool {
	return ignoredDirsAnywhere[name] || ignoredDirsTopOnly[name]
}

// ShouldSkipDirAt is the path-aware skip decision. Directories in the
// "anywhere" set are always skipped; directories in the "top-only" set
// are skipped only when their parent is the repo root. fullPath is the
// directory's absolute or repo-relative path; root is the repo root in
// the same form.
func ShouldSkipDirAt(root, fullPath string) bool {
	name := filepath.Base(fullPath)
	if ignoredDirsAnywhere[name] {
		return true
	}
	if ignoredDirsTopOnly[name] {
		return filepath.Clean(filepath.Dir(fullPath)) == filepath.Clean(root)
	}
	return false
}

// ShouldSkipFile returns true if the file should be ignored.
func ShouldSkipFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ignoredExts[ext] {
		return true
	}
	base := filepath.Base(path)
	// Skip dot-files that are not markdown.
	if strings.HasPrefix(base, ".") && !strings.HasSuffix(strings.ToLower(base), ".md") {
		return true
	}
	return false
}

// Walk visits every file under root that NeuroFS should index,
// calling fn for each. It respects the ignore rules in this package
// using the path-aware ShouldSkipDirAt so that names like "audit" only
// blanket-skip when they sit at root.
func Walk(root string, fn func(path string, info os.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if info.IsDir() {
			if ShouldSkipDirAt(root, path) {
				return filepath.SkipDir
			}
			return nil
		}
		if ShouldSkipFile(path) {
			return nil
		}
		return fn(path, info)
	})
}

// CountLines returns the number of lines in b.
// A trailing newline does not produce an extra empty line.
func CountLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	count := 0
	for _, c := range b {
		if c == '\n' {
			count++
		}
	}
	// If the last byte is not a newline, the final line has no terminator.
	if b[len(b)-1] != '\n' {
		count++
	}
	return count
}

// RelPath returns path relative to root; falls back to path on error.
func RelPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
