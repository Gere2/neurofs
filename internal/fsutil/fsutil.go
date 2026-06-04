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

// ignoredExts contains file extensions that carry no useful code
// context. Includes cryptographic material whose contents are secrets
// (HIGH-1 from the security traffic agent: a .pem in the repo was
// landing verbatim in pack bundles — fatal for the governance pitch).
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
	// Cryptographic / credential material — never source code.
	".pem":  true,
	".key":  true,
	".crt":  true,
	".cer":  true,
	".p12":  true,
	".pfx":  true,
	".kdbx": true, // KeePass database
	".gpg":  true,
	".asc":  true,
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

// ShouldSkipFile returns true if the file should be ignored — by
// extension (binaries, secrets, build artefacts), by dot-prefix
// (.env, .npmrc, .htpasswd, anything except .md), or by the
// secret-file heuristic for non-dot config files (secrets.yaml,
// credentials.json, etc.).
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
	if looksLikeSecretFile(base) {
		return true
	}
	return false
}

// secretBasenames are exact basenames whose contents are conventionally
// secrets. The heuristic is name-only — we never read the file to look
// for "high-entropy strings" because false positives at scan time are
// too costly.
var secretBasenames = map[string]bool{
	"id_rsa":     true,
	"id_dsa":     true,
	"id_ed25519": true,
	"id_ecdsa":   true,
	"kubeconfig": true,
	"htpasswd":   true,
}

// secretPrefixes are basename prefixes that, combined with a
// config-shaped extension, mark a file as secret-bearing. A file like
// secrets.go or credentials.ts is left alone — it is source code about
// secrets, not the secrets themselves — but secrets.production.yaml or
// credentials-prod.json is skipped.
var secretPrefixes = []string{
	"secrets.",
	"secrets-",
	"credentials.",
	"credentials-",
	"service-account.",
	"service_account.",
}

var secretConfigExts = []string{".json", ".yaml", ".yml", ".toml", ".env", ".ini"}

// looksLikeSecretFile flags basenames that conventionally store secret
// material in config formats. The check is conservative on purpose:
// source code with secret-flavoured names ("secrets.go") stays
// indexable — only config-shaped files (json/yaml/toml/env/ini)
// matching a secret prefix are blocked. The security traffic agent
// surfaced HIGH-1: config/secrets.yaml was landing verbatim in pack
// bundles, which kills the governance pitch.
func looksLikeSecretFile(base string) bool {
	lower := strings.ToLower(base)
	if secretBasenames[lower] {
		return true
	}
	for _, p := range secretPrefixes {
		if !strings.HasPrefix(lower, p) {
			continue
		}
		for _, e := range secretConfigExts {
			if strings.HasSuffix(lower, e) {
				return true
			}
		}
	}
	return false
}

// IgnoreMatcher holds rules parsed from .neurofsignore to skip files/directories.
type IgnoreMatcher struct {
	patterns []string
}

// LoadIgnoreMatcher loads patterns from .neurofsignore in the repo root.
func LoadIgnoreMatcher(repoRoot string) *IgnoreMatcher {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".neurofsignore"))
	if err != nil {
		return &IgnoreMatcher{}
	}
	var patterns []string
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Normalize to forward slashes for cross-platform matches
		patterns = append(patterns, filepath.ToSlash(line))
	}
	return &IgnoreMatcher{patterns: patterns}
}

// Match returns true if the relative path matches any ignore patterns.
func (m *IgnoreMatcher) Match(relPath string, isDir bool) bool {
	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")

	for _, pat := range m.patterns {
		isDirPattern := strings.HasSuffix(pat, "/")
		cleanPat := strings.TrimSuffix(pat, "/")

		if isDirPattern && !isDir {
			continue
		}

		// Case 1: Simple pattern with no slashes (e.g., "node_modules", "*_backup")
		if !strings.Contains(cleanPat, "/") {
			for _, part := range parts {
				matched, err := filepath.Match(cleanPat, part)
				if err == nil && matched {
					return true
				}
			}
			continue
		}

		// Case 2: Pattern contains slashes. Match against relative path.
		hasRootSlash := strings.HasPrefix(cleanPat, "/")
		matchPat := strings.TrimPrefix(cleanPat, "/")

		if hasRootSlash {
			matched, err := filepath.Match(matchPat, relPath)
			if err == nil && matched {
				return true
			}
			if strings.HasPrefix(relPath, matchPat+"/") {
				return true
			}
		} else {
			if strings.Contains(relPath, matchPat) {
				idx := strings.Index(relPath, matchPat)
				if idx >= 0 {
					leftOk := idx == 0 || relPath[idx-1] == '/'
					rightOk := idx+len(matchPat) == len(relPath) || relPath[idx+len(matchPat)] == '/'
					if leftOk && rightOk {
						return true
					}
				}
			}
		}
	}
	return false
}

// Walk visits every file under root that NeuroFS should index,
// calling fn for each. It respects the ignore rules in this package,
// including .neurofsignore rules.
func Walk(root string, fn func(path string, info os.FileInfo) error) error {
	matcher := LoadIgnoreMatcher(root)
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		relPath := RelPath(root, path)
		if info.IsDir() {
			if relPath != "." && matcher.Match(relPath, true) {
				return filepath.SkipDir
			}
			if ShouldSkipDirAt(root, path) {
				return filepath.SkipDir
			}
			return nil
		}
		if matcher.Match(relPath, false) {
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
