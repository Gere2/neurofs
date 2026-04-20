// Package project extracts structural signals from a repository's
// package.json and tsconfig.json, so ranking can weight entry points,
// dependencies and path aliases more intelligently than a raw file walk.
package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Info captures the bits of a TypeScript/Node project that help ranking.
// All fields are optional — an empty Info means "no project metadata found".
type Info struct {
	Name            string            `json:"name,omitempty"`
	Version         string            `json:"version,omitempty"`
	Main            string            `json:"main,omitempty"`
	Module          string            `json:"module,omitempty"`
	Types           string            `json:"types,omitempty"`
	BinEntries      []string          `json:"bin_entries,omitempty"`
	Scripts         map[string]string `json:"scripts,omitempty"`
	Dependencies    []string          `json:"dependencies,omitempty"`
	DevDependencies []string          `json:"dev_dependencies,omitempty"`

	// PathAliases maps a tsconfig paths key (trimmed of `/*`) to its target
	// directory (trimmed of `/*`). Example: "@app/*" → "src/*" becomes
	// {"@app": "src"}.
	PathAliases map[string]string `json:"path_aliases,omitempty"`
	BaseURL     string            `json:"base_url,omitempty"`

	// Sources records which files we actually read — makes `stats` explainable.
	Sources []string `json:"sources,omitempty"`
}

// Scan reads package.json and tsconfig.json from the repo root (if present)
// and returns an aggregated Info. Errors reading individual files are
// ignored — missing/broken config should degrade ranking gracefully, not
// fail the whole scan.
func Scan(repoRoot string) Info {
	var info Info
	if repoRoot == "" {
		return info
	}

	if pkg := readPackageJSON(filepath.Join(repoRoot, "package.json")); pkg != nil {
		info.Name = pkg.Name
		info.Version = pkg.Version
		info.Main = pkg.Main
		info.Module = pkg.Module
		info.Types = firstNonEmpty(pkg.Types, pkg.Typings)
		info.BinEntries = extractBinEntries(pkg.Bin)
		info.Scripts = pkg.Scripts
		info.Dependencies = sortedKeys(pkg.Dependencies)
		info.DevDependencies = sortedKeys(pkg.DevDependencies)
		info.Sources = append(info.Sources, "package.json")
	}

	if ts := readTSConfig(filepath.Join(repoRoot, "tsconfig.json")); ts != nil {
		info.BaseURL = ts.CompilerOptions.BaseURL
		info.PathAliases = normalisePaths(ts.CompilerOptions.Paths)
		info.Sources = append(info.Sources, "tsconfig.json")
	}

	return info
}

// Encode serialises Info as JSON for persistence in the metadata table.
// Returns "" when info is empty to avoid polluting the DB.
func (i Info) Encode() string {
	if i.IsEmpty() {
		return ""
	}
	b, err := json.Marshal(i)
	if err != nil {
		return ""
	}
	return string(b)
}

// Decode parses a previously-encoded Info from the metadata table.
// Returns nil when the input is empty or invalid.
func Decode(raw string) *Info {
	if raw == "" {
		return nil
	}
	var i Info
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		return nil
	}
	return &i
}

// IsEmpty returns true when nothing meaningful was extracted.
func (i Info) IsEmpty() bool {
	return i.Name == "" && i.Main == "" && i.Module == "" &&
		len(i.Dependencies) == 0 && len(i.DevDependencies) == 0 &&
		len(i.PathAliases) == 0 && len(i.BinEntries) == 0
}

// EntryPoints returns the relative paths declared as project entry points
// (main, module, types, bin). Callers use this to boost files that sit at
// the top of the dependency tree.
func (i Info) EntryPoints() []string {
	var out []string
	for _, p := range []string{i.Main, i.Module, i.Types} {
		if p != "" {
			out = append(out, normaliseEntry(p))
		}
	}
	for _, b := range i.BinEntries {
		if b != "" {
			out = append(out, normaliseEntry(b))
		}
	}
	return out
}

// Label returns a short human-readable description for display in `stats`.
func (i Info) Label() string {
	if i.Name == "" {
		return "(unnamed)"
	}
	if i.Version == "" {
		return i.Name
	}
	return i.Name + "@" + i.Version
}

// ─── raw file decoders ───────────────────────────────────────────────────────

type packageJSON struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	Main            string            `json:"main"`
	Module          string            `json:"module"`
	Types           string            `json:"types"`
	Typings         string            `json:"typings"`
	Bin             json.RawMessage   `json:"bin"` // string or object
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func readPackageJSON(path string) *packageJSON {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p packageJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	return &p
}

type tsConfig struct {
	CompilerOptions struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

func readTSConfig(path string) *tsConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Strip // and /* */ comments tsconfig allows but strict JSON doesn't.
	cleaned := stripJSONComments(data)
	var t tsConfig
	if err := json.Unmarshal(cleaned, &t); err != nil {
		return nil
	}
	return &t
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func extractBinEntries(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// Try string form first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			return []string{s}
		}
		return nil
	}
	// Otherwise expect map[string]string.
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil {
		out := make([]string, 0, len(m))
		for _, v := range m {
			if v != "" {
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normalisePaths(paths map[string][]string) map[string]string {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]string, len(paths))
	for alias, targets := range paths {
		if len(targets) == 0 {
			continue
		}
		key := strings.TrimSuffix(alias, "/*")
		key = strings.TrimSuffix(key, "*")
		val := strings.TrimSuffix(targets[0], "/*")
		val = strings.TrimSuffix(val, "*")
		out[key] = val
	}
	return out
}

func normaliseEntry(p string) string {
	p = strings.TrimPrefix(p, "./")
	return filepath.ToSlash(p)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// stripJSONComments removes // line and /* block */ comments from JSON bytes.
// It's intentionally simple — handles tsconfig.json's typical shape, not
// every edge case a generic JSON-with-comments parser would. Strings are
// respected so // inside a string literal is not treated as a comment.
func stripJSONComments(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString := false
	i := 0
	for i < len(in) {
		c := in[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(in) {
				out = append(out, in[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			i++
			continue
		}
		if c == '/' && i+1 < len(in) {
			if in[i+1] == '/' {
				// skip to end-of-line
				for i < len(in) && in[i] != '\n' {
					i++
				}
				continue
			}
			if in[i+1] == '*' {
				i += 2
				for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
					i++
				}
				i += 2
				continue
			}
		}
		out = append(out, c)
		i++
	}
	return out
}
