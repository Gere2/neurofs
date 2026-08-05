// Package project extracts structural signals from common root manifests so
// ranking can weight entry points, dependencies and aliases more intelligently
// than a raw file walk.
package project

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gere2/neurofs/internal/fsutil"
)

// Info captures language-agnostic project metadata plus a small set of
// ecosystem-specific fields. Existing JSON field names are intentionally kept
// stable; new fields are optional so previously persisted values still decode.
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
	// OptionalDependencies records packages declared in optional Python
	// dependency groups without treating them as required runtime dependencies.
	OptionalDependencies []string `json:"optional_dependencies,omitempty"`

	GoModule  string `json:"go_module,omitempty"`
	GoVersion string `json:"go_version,omitempty"`

	PythonRequires string            `json:"python_requires,omitempty"`
	PythonScripts  map[string]string `json:"python_scripts,omitempty"`

	RustEdition string `json:"rust_edition,omitempty"`
	RustVersion string `json:"rust_version,omitempty"`

	// PathAliases maps a tsconfig paths key (trimmed of `/*`) to its target
	// directory (trimmed of `/*`). Example: "@app/*" → "src/*" becomes
	// {"@app": "src"}.
	PathAliases map[string]string `json:"path_aliases,omitempty"`
	BaseURL     string            `json:"base_url,omitempty"`

	// Sources records which files we actually read — makes `stats` explainable.
	Sources []string `json:"sources,omitempty"`
}

const maxManifestBytes = int64(4 << 20)

// Scan reads supported root manifests and returns aggregated metadata. Errors
// in any individual file are ignored: a missing or malformed manifest must
// never make repository scanning fail.
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

	var goName, pythonName, pythonVersion, cargoName, cargoVersion string

	if mod := readGoMod(filepath.Join(repoRoot, "go.mod")); mod != nil {
		info.GoModule = mod.Module
		info.GoVersion = mod.GoVersion
		info.Dependencies = mergeSorted(info.Dependencies, mod.Dependencies)
		goName = moduleBase(mod.Module)
		info.Sources = append(info.Sources, "go.mod")
	}

	if py := readPyProject(filepath.Join(repoRoot, "pyproject.toml")); py != nil {
		info.PythonRequires = py.RequiresPython
		info.PythonScripts = py.Scripts
		info.Dependencies = mergeSorted(info.Dependencies, py.Dependencies)
		info.DevDependencies = mergeSorted(info.DevDependencies, py.DevDependencies)
		info.OptionalDependencies = mergeSorted(info.OptionalDependencies, py.OptionalDependencies)
		info.BinEntries = appendUnique(info.BinEntries, resolvePythonEntries(repoRoot, py.Scripts)...)
		pythonName = py.Name
		pythonVersion = py.Version
		info.Sources = append(info.Sources, "pyproject.toml")
	}

	if cargo := readCargoManifest(filepath.Join(repoRoot, "Cargo.toml"), repoRoot); cargo != nil {
		info.RustEdition = cargo.Edition
		info.RustVersion = cargo.RustVersion
		info.Dependencies = mergeSorted(info.Dependencies, cargo.Dependencies)
		info.DevDependencies = mergeSorted(info.DevDependencies, cargo.DevDependencies)
		info.BinEntries = appendUnique(info.BinEntries, cargo.EntryPoints...)
		cargoName = cargo.Name
		cargoVersion = cargo.Version
		info.Sources = append(info.Sources, "Cargo.toml")
	}

	if info.Name == "" {
		// Prefer explicit package metadata over a basename derived from a Go
		// module path when several ecosystems coexist at the repository root.
		switch {
		case pythonName != "":
			info.Name = pythonName
			info.Version = pythonVersion
		case cargoName != "":
			info.Name = cargoName
			info.Version = cargoVersion
		default:
			info.Name = goName
			info.Version = ""
		}
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
		len(i.OptionalDependencies) == 0 &&
		len(i.PathAliases) == 0 && len(i.BinEntries) == 0 &&
		i.GoModule == "" && i.GoVersion == "" &&
		i.PythonRequires == "" && len(i.PythonScripts) == 0 &&
		i.RustEdition == "" && i.RustVersion == ""
}

// EntryPoints returns the relative paths declared as project entry points
// (main, module, types, bin). Callers use this to boost files that sit at
// the top of the dependency tree.
func (i Info) EntryPoints() []string {
	var out []string
	for _, p := range []string{i.Main, i.Module, i.Types} {
		if entry := normaliseEntry(p); entry != "" {
			out = append(out, entry)
		}
	}
	for _, b := range i.BinEntries {
		if entry := normaliseEntry(b); entry != "" {
			out = append(out, entry)
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
	data, _, err := fsutil.ReadRegularFileBounded(path, maxManifestBytes)
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
	data, _, err := fsutil.ReadRegularFileBounded(path, maxManifestBytes)
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
	p = filepath.Clean(filepath.FromSlash(strings.TrimSpace(p)))
	if p == "." || filepath.IsAbs(p) || p == ".." ||
		strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return ""
	}
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
				for i+1 < len(in) && (in[i] != '*' || in[i+1] != '/') {
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
