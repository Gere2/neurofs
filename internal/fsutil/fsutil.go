// Package fsutil provides file-system helpers for NeuroFS.
package fsutil

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// ignoredDirs contains directory names that are always skipped.
var ignoredDirs = map[string]bool{
	".git":         true,
	".neurofs":     true,
	".claude":      true, // Claude Code metadata + ephemeral worktrees under .claude/worktrees/**
	"audit":        true, // NeuroFS's own audit/{bundles,records,responses,notes} — re-ingesting our outputs poisons ranking
	".audit":       true,
	"testdata":     true, // Go convention: testdata/ is fixtures, not the project's own code — ignored by `go build` and should be by ranking too
	"fixtures":     true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".mypy_cache":  true,
	".pytest_cache": true,
	"coverage":     true,
	".coverage":    true,
	"target":       true, // Rust / Java
	"out":          true,
	".idea":        true,
	".vscode":      true,
}

// ignoredExts contains file extensions that carry no useful code context.
var ignoredExts = map[string]bool{
	".png":   true,
	".jpg":   true,
	".jpeg":  true,
	".gif":   true,
	".svg":   true,
	".ico":   true,
	".woff":  true,
	".woff2": true,
	".ttf":   true,
	".eot":   true,
	".mp4":   true,
	".mp3":   true,
	".zip":   true,
	".tar":   true,
	".gz":    true,
	".lock":  true,
	".sum":   true,
	".pdf":   true,
	".bin":   true,
	".exe":   true,
	".dll":   true,
	".so":    true,
	".dylib": true,
	".map":   true, // source maps
	".min.js": true,
}

// extToLang maps file extensions to a Lang value.
var extToLang = map[string]models.Lang{
	".ts":   models.LangTypeScript,
	".tsx":  models.LangTypeScript,
	".mts":  models.LangTypeScript,
	".js":   models.LangJavaScript,
	".jsx":  models.LangJavaScript,
	".mjs":  models.LangJavaScript,
	".cjs":  models.LangJavaScript,
	".py":   models.LangPython,
	".go":   models.LangGo,
	".md":   models.LangMarkdown,
	".mdx":  models.LangMarkdown,
	".json": models.LangJSON,
	".yaml": models.LangYAML,
	".yml":  models.LangYAML,
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

// ShouldSkipDir returns true if the directory should be ignored entirely.
func ShouldSkipDir(name string) bool {
	return ignoredDirs[name]
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
// calling fn for each. It respects the ignore rules in this package.
func Walk(root string, fn func(path string, info os.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if info.IsDir() {
			if ShouldSkipDir(info.Name()) {
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
