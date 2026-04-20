package audit

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// citationPattern matches file-like references in free text. It is
// intentionally conservative: it requires a known source extension so prose
// containing a dotted phrase ("e.g.") is not pulled in as a citation. Line
// numbers are optional and captured separately.
//
// Accepted extensions are the subset NeuroFS already indexes.
var citationPattern = regexp.MustCompile(
	`([A-Za-z0-9_./\-]+\.(?:ts|tsx|js|jsx|mjs|cjs|py|go|md|json|ya?ml))(?::(\d+))?`,
)

// ParseCitations extracts every file reference from a model response. Each
// citation is returned with Valid=false; call ValidateCitations to cross
// them against a bundle. Duplicates (same path+line) are collapsed so the
// grounded ratio is not inflated by the model repeating itself.
func ParseCitations(text string) []Citation {
	matches := citationPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(matches))
	out := make([]Citation, 0, len(matches))
	for _, m := range matches {
		raw := m[0]
		rel := strings.TrimPrefix(m[1], "./")
		line := 0
		if len(m) > 2 && m[2] != "" {
			if n, err := strconv.Atoi(m[2]); err == nil {
				line = n
			}
		}
		key := rel + ":" + strconv.Itoa(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Citation{Raw: raw, RelPath: rel, Line: line})
	}
	return out
}

// ValidateCitations marks each citation as Valid when the bundle contains a
// fragment with a matching relpath. Path matching is case-sensitive and uses
// forward slashes (packager already normalises that way). We also accept a
// basename match so responses that refer to a file by name alone
// ("see crypto.ts") still get credit when unambiguous.
func ValidateCitations(cs []Citation, bundle models.Bundle) []Citation {
	if len(cs) == 0 {
		return cs
	}
	relSet := make(map[string]bool, len(bundle.Fragments))
	baseIndex := make(map[string][]string, len(bundle.Fragments))
	for _, f := range bundle.Fragments {
		p := filepath.ToSlash(f.RelPath)
		relSet[p] = true
		base := filepath.Base(p)
		baseIndex[base] = append(baseIndex[base], p)
	}

	out := make([]Citation, len(cs))
	for i, c := range cs {
		out[i] = c
		path := filepath.ToSlash(c.RelPath)
		if relSet[path] {
			out[i].Valid = true
			continue
		}
		// Fall back to basename: only accept when a single fragment claims it,
		// otherwise the citation is ambiguous and we refuse to validate.
		matches := baseIndex[filepath.Base(path)]
		switch len(matches) {
		case 1:
			out[i].Valid = true
			out[i].RelPath = matches[0] // normalise for downstream consumers
		case 0:
			out[i].Reason = "not in bundle"
		default:
			out[i].Reason = "ambiguous basename"
		}
	}
	return out
}

// GroundedRatio returns valid_citations / total_citations. Returns 1.0 when
// there are no citations at all — "no claims, no drift" — and 0.0 when
// there are claims but none validated.
func GroundedRatio(cs []Citation) float64 {
	if len(cs) == 0 {
		return 1.0
	}
	valid := 0
	for _, c := range cs {
		if c.Valid {
			valid++
		}
	}
	return float64(valid) / float64(len(cs))
}
