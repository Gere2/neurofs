// Package contextmap builds compact file-level logic maps from the index.
package contextmap

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// SymbolRange is the indexed, line-addressable view of a symbol or chunk.
type SymbolRange struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	StartLine     int      `json:"start_line"`
	EndLine       int      `json:"end_line"`
	ContentHash   string   `json:"content_hash,omitempty"`
	TokenEstimate int      `json:"token_estimate,omitempty"`
	Calls         []string `json:"calls,omitempty"`
}

// LogicMap describes what an agent needs before deciding whether to expand.
type LogicMap struct {
	Path         string        `json:"path"`
	Lang         models.Lang   `json:"lang"`
	Lines        int           `json:"lines"`
	SizeBytes    int64         `json:"size_bytes"`
	Checksum     string        `json:"checksum,omitempty"`
	Symbols      []SymbolRange `json:"symbols,omitempty"`
	Imports      []string      `json:"imports,omitempty"`
	Dependencies []string      `json:"dependencies,omitempty"`
	Dependents   []string      `json:"dependents,omitempty"`
	RelatedTests []string      `json:"related_tests,omitempty"`
}

// Build creates a logic map for rec using already-loaded index facts.
func Build(rec models.FileRecord, files []models.FileRecord, chunks []models.Chunk, rels []models.FileRelation, content string) LogicMap {
	relByAbs := relPathByAbs(files)
	out := LogicMap{
		Path:      rec.RelPath,
		Lang:      rec.Lang,
		Lines:     rec.Lines,
		SizeBytes: rec.Size,
		Checksum:  rec.Checksum,
		Imports:   append([]string(nil), rec.Imports...),
	}

	for _, c := range chunks {
		calls := c.Calls
		if len(calls) == 0 && content != "" {
			calls = CallsIn(LinesInRange(content, c.StartLine, c.EndLine))
		}
		out.Symbols = append(out.Symbols, SymbolRange{
			Name:          c.Symbol,
			Kind:          c.Kind,
			StartLine:     c.StartLine,
			EndLine:       c.EndLine,
			ContentHash:   c.ContentHash,
			TokenEstimate: c.TokenEstimate,
			Calls:         calls,
		})
	}
	if len(out.Symbols) == 0 {
		for _, s := range rec.Symbols {
			out.Symbols = append(out.Symbols, SymbolRange{
				Name:      s.Name,
				Kind:      s.Kind,
				StartLine: s.Line,
				EndLine:   s.Line,
			})
		}
	}
	sort.Slice(out.Symbols, func(i, j int) bool {
		if out.Symbols[i].StartLine != out.Symbols[j].StartLine {
			return out.Symbols[i].StartLine < out.Symbols[j].StartLine
		}
		return out.Symbols[i].Name < out.Symbols[j].Name
	})

	for _, rel := range rels {
		if rel.SourcePath == rec.Path {
			out.Dependencies = append(out.Dependencies, relName(rel.TargetPath, relByAbs))
		}
		if rel.TargetPath == rec.Path {
			out.Dependents = append(out.Dependents, relName(rel.SourcePath, relByAbs))
		}
	}
	out.Dependencies = sortedUnique(out.Dependencies)
	out.Dependents = sortedUnique(out.Dependents)
	out.RelatedTests = RelatedTests(rec, files, rels, relByAbs)
	return out
}

// LinesInRange returns 1-based inclusive source lines from content.
func LinesInRange(content string, startLine, endLine int) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine || startLine > len(lines) {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}

var callRE = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\s*\(`)

var callStop = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"func": true, "function": true, "def": true, "class": true,
	"var": true, "const": true, "type": true,
	"len": true, "cap": true, "make": true, "new": true, "byte": true,
	"by": true, "flow": true, "fresh": true, "only": true, "root": true, "summary": true,
}

// CallsIn extracts a conservative list of call-like identifiers.
func CallsIn(content string) []string {
	matches := callRE.FindAllStringSubmatch(content, -1)
	calls := make([]string, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if name == "" || callStop[strings.ToLower(name)] {
			continue
		}
		calls = append(calls, name)
	}
	return sortedUnique(calls)
}

// RelatedTests returns indexed test-like files close to rec by imports or name.
func RelatedTests(rec models.FileRecord, files []models.FileRecord, rels []models.FileRelation, relByAbs map[string]string) []string {
	tests := make([]string, 0)
	base := productionStem(rec.RelPath)
	for _, f := range files {
		if !IsTestPath(f.RelPath) {
			continue
		}
		if productionStem(f.RelPath) == base || strings.Contains(productionStem(f.RelPath), base) || strings.Contains(base, productionStem(f.RelPath)) {
			tests = append(tests, f.RelPath)
		}
	}
	for _, rel := range rels {
		if rel.TargetPath == rec.Path {
			if relPath, ok := relByAbs[rel.SourcePath]; ok && IsTestPath(relPath) {
				tests = append(tests, relPath)
			}
		}
		if IsTestPath(rec.RelPath) && rel.SourcePath == rec.Path {
			if relPath, ok := relByAbs[rel.TargetPath]; ok {
				tests = append(tests, relPath)
			}
		}
	}
	return sortedUnique(tests)
}

// IsTestPath reports common test/spec file names.
func IsTestPath(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)
	return strings.Contains(p, "/test/") ||
		strings.Contains(p, "/tests/") ||
		strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.")
}

func productionStem(path string) string {
	p := filepath.ToSlash(path)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for _, suffix := range []string{"_test", ".test", ".spec"} {
		stem = strings.TrimSuffix(stem, suffix)
	}
	return strings.ToLower(stem)
}

func relPathByAbs(files []models.FileRecord) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = f.RelPath
	}
	return out
}

func relName(abs string, relByAbs map[string]string) string {
	if rel, ok := relByAbs[abs]; ok {
		return rel
	}
	return abs
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
