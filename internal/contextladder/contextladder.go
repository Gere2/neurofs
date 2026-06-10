// Package contextladder contains shared expansion primitives for agents.
package contextladder

import (
	"crypto/sha256"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/neuromfs/neuromfs/internal/contextmap"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

// Mode selects one step of the context ladder.
type Mode string

const (
	ModeAuto    Mode = "auto"
	ModeOutline Mode = "outline"
	ModeExcerpt Mode = "excerpt"
	ModeFull    Mode = "full"
)

// Spec is a line-addressable expansion target.
type Spec struct {
	Path      string
	StartLine int
	EndLine   int
	Hash      string
}

// ExpandedContent is the machine-readable payload for excerpt/full expansion.
type ExpandedContent struct {
	Mode        Mode   `json:"mode"`
	Path        string `json:"path"`
	StartLine   int    `json:"start_line,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Tokens      int    `json:"tokens"`
	Content     string `json:"content"`
}

var expandRangeRE = regexp.MustCompile(`^(.+):([0-9]+)(?:-([0-9]+))?$`)

// ParseSpec parses path, path:start-end, or hash-looking targets.
func ParseSpec(raw string) Spec {
	raw = strings.TrimSpace(raw)
	if m := expandRangeRE.FindStringSubmatch(raw); m != nil {
		start, _ := strconv.Atoi(m[2])
		end := start
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
		return Spec{Path: filepath.ToSlash(m[1]), StartLine: start, EndLine: end}
	}
	return Spec{Path: filepath.ToSlash(raw)}
}

// EffectiveMode maps auto to outline or excerpt based on the target.
func EffectiveMode(mode string, spec Spec) (Mode, error) {
	m := Mode(strings.TrimSpace(strings.ToLower(mode)))
	switch m {
	case "", ModeAuto:
		if spec.StartLine > 0 || spec.Hash != "" {
			return ModeExcerpt, nil
		}
		return ModeOutline, nil
	case ModeOutline, ModeExcerpt, ModeFull:
		return m, nil
	default:
		return "", fmt.Errorf("unknown mode %q (want auto, outline, excerpt, full)", mode)
	}
}

// ResolveSpec resolves a path/range/hash target against indexed files/chunks.
func ResolveSpec(db *storage.DB, files []models.FileRecord, spec Spec) (models.FileRecord, Spec, error) {
	if spec.Hash != "" && spec.Path == "" {
		return resolveChunkHash(db, files, spec)
	}
	if rec, ok := FindFile(files, spec.Path); ok {
		if spec.Hash != "" {
			if chunk, ok := chunkByHash(db, spec.Hash); ok {
				if chunk.FilePath != rec.Path {
					return models.FileRecord{}, spec, fmt.Errorf("hash %s belongs to another file", spec.Hash)
				}
				if spec.StartLine == 0 {
					spec.StartLine, spec.EndLine = chunk.StartLine, chunk.EndLine
				}
			} else if spec.Hash != rec.Checksum {
				return models.FileRecord{}, spec, fmt.Errorf("hash mismatch for %s: %s", rec.RelPath, spec.Hash)
			}
		}
		return rec, spec, nil
	}
	if spec.Path != "" {
		if rec, resolved, ok := resolveHashIfTarget(db, files, spec.Path); ok {
			spec.Hash = spec.Path
			spec.Path = resolved.Path
			spec.StartLine = resolved.StartLine
			spec.EndLine = resolved.EndLine
			return rec, spec, nil
		}
	}
	return models.FileRecord{}, spec, fmt.Errorf("indexed file or chunk hash not found: %s", spec.Path)
}

func resolveChunkHash(db *storage.DB, files []models.FileRecord, spec Spec) (models.FileRecord, Spec, error) {
	rec, chunk, ok := chunkRecordByHash(db, files, spec.Hash)
	if !ok {
		return models.FileRecord{}, spec, fmt.Errorf("chunk hash not found: %s", spec.Hash)
	}
	spec.Path = rec.RelPath
	spec.StartLine = chunk.StartLine
	spec.EndLine = chunk.EndLine
	return rec, spec, nil
}

func resolveHashIfTarget(db *storage.DB, files []models.FileRecord, hash string) (models.FileRecord, Spec, bool) {
	rec, chunk, ok := chunkRecordByHash(db, files, hash)
	if !ok {
		return models.FileRecord{}, Spec{}, false
	}
	return rec, Spec{Path: rec.RelPath, Hash: hash, StartLine: chunk.StartLine, EndLine: chunk.EndLine}, true
}

func chunkRecordByHash(db *storage.DB, files []models.FileRecord, hash string) (models.FileRecord, models.Chunk, bool) {
	chunk, ok := chunkByHash(db, hash)
	if !ok {
		return models.FileRecord{}, models.Chunk{}, false
	}
	for _, rec := range files {
		if rec.Path == chunk.FilePath {
			return rec, chunk, true
		}
	}
	return models.FileRecord{}, models.Chunk{}, false
}

func chunkByHash(db *storage.DB, hash string) (models.Chunk, bool) {
	chunks, err := db.SearchChunks(storage.ChunkSearchOptions{ContentHash: hash, Limit: 1})
	if err != nil || len(chunks) == 0 {
		return models.Chunk{}, false
	}
	return chunks[0], true
}

// FindFile matches repo-relative or absolute paths from an indexed file list.
func FindFile(files []models.FileRecord, path string) (models.FileRecord, bool) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return models.FileRecord{}, false
	}
	for _, f := range files {
		if filepath.ToSlash(f.RelPath) == path || filepath.ToSlash(f.Path) == path {
			return f, true
		}
	}
	return models.FileRecord{}, false
}

// BuildExcerpt returns the current content for a line range, validating hashes
// against both indexed coverage and live file content when a hash is supplied.
func BuildExcerpt(rec models.FileRecord, chunks []models.Chunk, content string, spec Spec) (ExpandedContent, error) {
	start, end := spec.StartLine, spec.EndLine
	hash := spec.Hash
	if start <= 0 {
		if hash == "" {
			return ExpandedContent{}, fmt.Errorf("excerpt mode needs a line range or hash")
		}
		for _, c := range chunks {
			if c.ContentHash == hash {
				start, end = c.StartLine, c.EndLine
				break
			}
		}
	}
	if end <= 0 {
		end = start
	}
	if start <= 0 || end < start {
		return ExpandedContent{}, fmt.Errorf("invalid line range %d-%d", start, end)
	}
	rawRangeContent := contextmap.LinesInRange(content, start, end)
	if rawRangeContent == "" {
		return ExpandedContent{}, fmt.Errorf("range %d-%d is empty in %s", start, end, rec.RelPath)
	}
	currentHash := SHA256Hex(rawRangeContent)
	rangeContent := strings.TrimRight(rawRangeContent, "\n")
	if hash != "" {
		matched := false
		for _, c := range chunks {
			if c.ContentHash == hash && c.StartLine == start && c.EndLine == end {
				matched = true
				break
			}
		}
		if !matched {
			return ExpandedContent{}, fmt.Errorf("hash %s does not cover %s:%d-%d", hash, rec.RelPath, start, end)
		}
		if currentHash != hash {
			return ExpandedContent{}, fmt.Errorf("hash mismatch for %s:%d-%d: expected %s, got %s", rec.RelPath, start, end, hash, currentHash)
		}
	} else {
		for _, c := range chunks {
			if c.StartLine == start && c.EndLine == end && c.ContentHash == currentHash {
				hash = c.ContentHash
				break
			}
		}
	}
	return ExpandedContent{
		Mode:        ModeExcerpt,
		Path:        rec.RelPath,
		StartLine:   start,
		EndLine:     end,
		ContentHash: hash,
		Tokens:      tokenbudget.EstimateTokens(rangeContent),
		Content:     rangeContent,
	}, nil
}

// BuildFull returns the current full file content, optionally validating hash.
func BuildFull(rec models.FileRecord, content string, spec Spec) (ExpandedContent, error) {
	currentFileHash := SHA256Hex(content)
	if spec.Hash != "" && spec.Hash != currentFileHash {
		return ExpandedContent{}, fmt.Errorf("hash mismatch for full file %s: expected %s, got %s", rec.RelPath, spec.Hash, currentFileHash)
	}
	return ExpandedContent{
		Mode:        ModeFull,
		Path:        rec.RelPath,
		StartLine:   1,
		EndLine:     rec.Lines,
		ContentHash: currentFileHash,
		Tokens:      tokenbudget.EstimateTokens(content),
		Content:     strings.TrimRight(content, "\n"),
	}, nil
}

// SHA256Hex returns a stable content hash compatible with indexed chunks.
func SHA256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

// EstimateOutlineTokens estimates the rendered text form of a logic map.
func EstimateOutlineTokens(logic contextmap.LogicMap) int {
	var sb strings.Builder
	WriteOutline(&sb, logic)
	return tokenbudget.EstimateTokens(sb.String())
}

// WriteOutline renders a compact, human-readable logic map.
func WriteOutline(w io.Writer, logic contextmap.LogicMap) {
	fmt.Fprintf(w, "<outline path=%q lang=%q lines=%d hash=%q>\n",
		logic.Path, logic.Lang, logic.Lines, logic.Checksum)
	if len(logic.Symbols) > 0 {
		fmt.Fprintln(w, "symbols:")
		for _, sym := range logic.Symbols {
			label := strings.TrimSpace(sym.Name)
			if label == "" {
				label = sym.Kind
			}
			fmt.Fprintf(w, "- %s %s L%d-L%d", sym.Kind, label, sym.StartLine, sym.EndLine)
			if sym.ContentHash != "" {
				fmt.Fprintf(w, " hash=%s", sym.ContentHash)
			}
			if len(sym.Calls) > 0 {
				fmt.Fprintf(w, " calls=%s", strings.Join(sym.Calls, ","))
			}
			fmt.Fprintln(w)
		}
	}
	writeStringList(w, "imports", logic.Imports)
	writeStringList(w, "dependencies", logic.Dependencies)
	writeStringList(w, "dependents", logic.Dependents)
	writeStringList(w, "related_tests", logic.RelatedTests)
	fmt.Fprintln(w, "next:")
	for _, sym := range logic.Symbols {
		label := strings.TrimSpace(sym.Name)
		if label == "" {
			label = sym.Kind
		}
		hashArg := ""
		if sym.ContentHash != "" {
			hashArg = " --hash " + sym.ContentHash
		}
		fmt.Fprintf(w, "- neurofs expand %s:%d-%d%s\n", logic.Path, sym.StartLine, sym.EndLine, hashArg)
		if label != sym.Kind {
			break
		}
	}
	fmt.Fprintf(w, "- neurofs expand %s --mode full\n", logic.Path)
	fmt.Fprintln(w, "</outline>")
}

func writeStringList(w io.Writer, name string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", name)
	for _, value := range values {
		fmt.Fprintf(w, "- %s\n", value)
	}
}

// WriteExpandedContent renders excerpt/full content for prompt inclusion.
func WriteExpandedContent(w io.Writer, out ExpandedContent) {
	tag := "excerpt"
	if out.Mode == ModeFull {
		tag = "file"
	}
	fmt.Fprintf(w, "<%s path=%q", tag, out.Path)
	if out.StartLine > 0 && out.EndLine >= out.StartLine {
		fmt.Fprintf(w, " start=%d end=%d", out.StartLine, out.EndLine)
	}
	if out.ContentHash != "" {
		fmt.Fprintf(w, " hash=%q", out.ContentHash)
	}
	fmt.Fprintf(w, " tokens=%d>\n", out.Tokens)
	fmt.Fprintf(w, "%s\n", out.Content)
	fmt.Fprintf(w, "</%s>\n", tag)
}
