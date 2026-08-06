package audit

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/models"
)

// citationPattern matches file-like references in free text. It is
// intentionally conservative: it requires a known source extension so prose
// containing a dotted phrase ("e.g.") is not pulled in as a citation. Line
// numbers are optional and captured separately.
//
// The regexp deliberately accepts a generic extension. ParseCitations then
// asks fsutil whether the path is actually supported, keeping citation parsing
// in lockstep with the scanner's language registry.
var citationPattern = regexp.MustCompile(
	`([A-Za-z0-9_./\-]+\.[A-Za-z0-9]+)(?::(\d+))?`,
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
		if !fsutil.IsSupported(rel) {
			continue
		}
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

type citationRange struct {
	start int
	end   int
}

// excerptRangePattern recognises the source-range markers emitted by the
// packager for discontiguous excerpts. ContextFragment only has one structured
// range, so the markers are the authoritative ranges for those fragments.
var excerptRangePattern = regexp.MustCompile(`(?m)^\s*//\s*──\s+[^:\n]+:(\d+)-(\d+)\b`)

// ValidateCitations marks a citation valid only when its path is in the bundle
// and, when a line is supplied, that line is actually visible in one of the
// bundle's source ranges. Path matching is case-sensitive and uses forward
// slashes. A basename is accepted only when it resolves to one unique path.
func ValidateCitations(cs []Citation, bundle models.Bundle) []Citation {
	if len(cs) == 0 {
		return cs
	}
	relSet := make(map[string]bool, len(bundle.Fragments))
	ranges := make(map[string][]citationRange, len(bundle.Fragments))
	basePaths := make(map[string]map[string]struct{}, len(bundle.Fragments))
	for _, f := range bundle.Fragments {
		p := filepath.ToSlash(f.RelPath)
		relSet[p] = true
		if f.StartLine > 0 && f.EndLine >= f.StartLine {
			ranges[p] = append(ranges[p], citationRange{start: f.StartLine, end: f.EndLine})
		}
		for _, match := range excerptRangePattern.FindAllStringSubmatch(f.Content, -1) {
			start, startErr := strconv.Atoi(match[1])
			end, endErr := strconv.Atoi(match[2])
			if startErr == nil && endErr == nil && start > 0 && end >= start {
				ranges[p] = append(ranges[p], citationRange{start: start, end: end})
			}
		}
		base := filepath.Base(p)
		if basePaths[base] == nil {
			basePaths[base] = make(map[string]struct{})
		}
		basePaths[base][p] = struct{}{}
	}

	out := make([]Citation, len(cs))
	for i, c := range cs {
		out[i] = c
		path := filepath.ToSlash(c.RelPath)
		if !relSet[path] {
			// Fall back to basename: count unique paths, not fragments. A
			// chunked bundle commonly contains several fragments of one file.
			matches := basePaths[filepath.Base(path)]
			switch len(matches) {
			case 1:
				for match := range matches {
					path = match
				}
				out[i].RelPath = path
			case 0:
				out[i].Reason = "not in bundle"
				continue
			default:
				out[i].Reason = "ambiguous basename"
				continue
			}
		}

		if c.Line == 0 {
			out[i].Valid = true
			continue
		}
		visible := ranges[path]
		if len(visible) == 0 {
			out[i].Reason = "bundle fragment has no source line range"
			continue
		}
		for _, r := range visible {
			if c.Line >= r.start && c.Line <= r.end {
				out[i].Valid = true
				break
			}
		}
		if !out[i].Valid {
			out[i].Reason = "line not present in bundle"
		}
	}
	return out
}

// GroundedRatio returns valid_citations / total_citations. An answer with no
// citations receives 0 rather than a perfect score: citation parsing cannot
// infer that prose contains no claims, so treating absence as grounded would
// be a dangerous false positive.
func GroundedRatio(cs []Citation) float64 {
	if len(cs) == 0 {
		return 0
	}
	valid := 0
	for _, c := range cs {
		if c.Valid {
			valid++
		}
	}
	return float64(valid) / float64(len(cs))
}
