package indexer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/contextmap"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/models"
	codeParser "github.com/neuromfs/neuromfs/internal/parser"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

var chunkIDUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// BuildChunks splits a file into stable, hashable code chunks. Go uses the
// standard AST for top-level declarations; other languages currently fall
// back to a whole-file chunk until their AST chunkers land.
func BuildChunks(filePath, relPath string, lang models.Lang, content string, indexedAt time.Time) []models.Chunk {
	var chunks []models.Chunk
	switch lang {
	case models.LangGo:
		chunks = buildGoChunks(filePath, relPath, content, indexedAt)
	case models.LangTypeScript, models.LangJavaScript:
		chunks = buildJSChunks(filePath, relPath, content, indexedAt)
	case models.LangPython:
		chunks = buildPythonChunks(filePath, relPath, content, indexedAt)
	}
	if len(chunks) == 0 {
		chunks = []models.Chunk{newChunk(filePath, "file", filepath.Base(relPath), 1, countLogicalLines(content), content, indexedAt)}
	}
	return uniqueChunkIDs(chunks)
}

func buildGoChunks(filePath, relPath, content string, indexedAt time.Time) []models.Chunk {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}

	lines := logicalLines(content)
	var chunks []models.Chunk
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			symbol := d.Name.Name
			kind := "func"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				if recv := receiverName(d.Recv.List[0].Type); recv != "" {
					symbol = recv + "." + symbol
					kind = "method"
				}
			}
			start := d.Pos()
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			chunks = append(chunks, newChunkFromLines(filePath, kind, symbol, fset.Position(start).Line, fset.Position(d.End()).Line, lines, indexedAt))
		case *ast.GenDecl:
			chunks = append(chunks, goGenDeclChunks(filePath, d, fset, lines, indexedAt)...)
		}
	}

	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].StartLine != chunks[j].StartLine {
			return chunks[i].StartLine < chunks[j].StartLine
		}
		return chunks[i].ChunkID < chunks[j].ChunkID
	})
	return chunks
}

func goGenDeclChunks(filePath string, d *ast.GenDecl, fset *token.FileSet, lines []string, indexedAt time.Time) []models.Chunk {
	if d.Tok == token.IMPORT {
		return nil
	}

	kind := strings.ToLower(d.Tok.String())
	var chunks []models.Chunk
	for i, spec := range d.Specs {
		symbol := specSymbol(spec)
		if symbol == "" {
			continue
		}
		start := spec.Pos()
		if doc := specDoc(spec); doc != nil {
			start = doc.Pos()
		} else if i == 0 && d.Doc != nil {
			start = d.Doc.Pos()
		}

		// Single-spec declarations are self-contained only when the keyword is
		// included, so use the GenDecl range for that common shape.
		end := spec.End()
		if len(d.Specs) == 1 {
			start = d.Pos()
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			end = d.End()
		}
		chunks = append(chunks, newChunkFromLines(filePath, kind, symbol, fset.Position(start).Line, fset.Position(end).Line, lines, indexedAt))
	}
	return chunks
}

func specSymbol(spec ast.Spec) string {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		if s.Name != nil {
			return s.Name.Name
		}
	case *ast.ValueSpec:
		if len(s.Names) == 0 {
			return ""
		}
		names := make([]string, 0, len(s.Names))
		for _, n := range s.Names {
			names = append(names, n.Name)
		}
		return strings.Join(names, ",")
	}
	return ""
}

func specDoc(s ast.Spec) *ast.CommentGroup {
	switch x := s.(type) {
	case *ast.TypeSpec:
		return x.Doc
	case *ast.ValueSpec:
		return x.Doc
	}
	return nil
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	}
	return ""
}

func newChunkFromLines(filePath, kind, symbol string, startLine, endLine int, lines []string, indexedAt time.Time) models.Chunk {
	content := linesInRange(lines, startLine, endLine)
	return newChunk(filePath, kind, symbol, startLine, endLine, content, indexedAt)
}

func newChunk(filePath, kind, symbol string, startLine, endLine int, content string, indexedAt time.Time) models.Chunk {
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	contentHash := sha256Hex(content)
	astHash := sha256Hex(fmt.Sprintf("%s\x00%s\x00%d\x00%d", kind, symbol, startLine, endLine))
	return models.Chunk{
		FilePath:      filePath,
		ChunkID:       makeChunkID(kind, symbol),
		Kind:          kind,
		Symbol:        symbol,
		StartLine:     startLine,
		EndLine:       endLine,
		Content:       content,
		ContentHash:   contentHash,
		ASTHash:       astHash,
		Calls:         contextmap.CallsIn(content),
		TokenEstimate: tokenbudget.EstimateTokens(content),
		IndexedAt:     indexedAt,
	}
}

func makeChunkID(kind, symbol string) string {
	base := strings.TrimSpace(strings.ToLower(kind + ":" + symbol))
	base = chunkIDUnsafe.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		return "chunk"
	}
	return base
}

func uniqueChunkIDs(chunks []models.Chunk) []models.Chunk {
	seen := make(map[string]int, len(chunks))
	for i := range chunks {
		base := chunks[i].ChunkID
		if base == "" {
			base = makeChunkID(chunks[i].Kind, chunks[i].Symbol)
		}
		seen[base]++
		if seen[base] == 1 {
			chunks[i].ChunkID = base
			continue
		}
		chunks[i].ChunkID = fmt.Sprintf("%s-%d", base, seen[base])
	}
	return chunks
}

func persistChunks(ctx context.Context, db *storage.DB, embClient *embeddings.Client, rec models.FileRecord, content string) (int, error) {
	chunks := BuildChunks(rec.Path, rec.RelPath, rec.Lang, content, rec.IndexedAt)
	if err := db.UpdateChunks(rec.Path, chunks); err != nil {
		return 0, err
	}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.Content) == "" {
			continue
		}
		if _, ok, err := db.GetChunkEmbedding(chunk.ContentHash); err != nil {
			return len(chunks), err
		} else if ok {
			continue
		}
		emb, err := embClient.GetEmbedding(ctx, chunk.Content)
		if err != nil {
			continue
		}
		if err := db.SaveChunkEmbedding(chunk.ContentHash, emb, embClient.ProviderName(), embClient.ModelName()); err != nil {
			return len(chunks), err
		}
	}
	return len(chunks), nil
}

func logicalLines(content string) []string {
	if content == "" {
		return []string{""}
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func countLogicalLines(content string) int {
	return len(logicalLines(content))
}

func linesInRange(lines []string, startLine, endLine int) string {
	if len(lines) == 0 {
		return ""
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func buildJSChunks(filePath, relPath, content string, indexedAt time.Time) []models.Chunk {
	res := codeParser.Parse(models.LangTypeScript, content)
	if len(res.Symbols) == 0 {
		return nil
	}

	lines := logicalLines(content)
	offsets := lineOffsets(content)
	var chunks []models.Chunk

	for _, sym := range res.Symbols {
		if sym.Line <= 0 || sym.Line > len(offsets) {
			continue
		}
		startOffset := offsets[sym.Line-1]
		braceOffset, semiOffset := findJSDeclEnd(content, startOffset)

		var endLine int
		if braceOffset >= 0 {
			_, bodyEnd := codeParser.FindBraceBody(content, braceOffset)
			if bodyEnd >= 0 {
				endLine = lineForOffset(offsets, bodyEnd)
			} else {
				endLine = sym.Line
			}
		} else if semiOffset >= 0 {
			endLine = lineForOffset(offsets, semiOffset)
		} else {
			endLine = sym.Line
		}

		if endLine < sym.Line {
			endLine = sym.Line
		}
		chunks = append(chunks, newChunkFromLines(filePath, sym.Kind, sym.Name, sym.Line, endLine, lines, indexedAt))
	}

	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].StartLine != chunks[j].StartLine {
			return chunks[i].StartLine < chunks[j].StartLine
		}
		return chunks[i].ChunkID < chunks[j].ChunkID
	})
	return chunks
}

func findJSDeclEnd(content string, startOffset int) (braceOffset int, semiOffset int) {
	inString := false
	var quote byte
	inLineComment := false
	inBlockComment := false

	for i := startOffset; i < len(content); i++ {
		c := content[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '/' && i > 0 && content[i-1] == '*' {
				inBlockComment = false
			}
			continue
		}
		if inString {
			if c == '\\' {
				i++ // skip next char
				continue
			}
			if c == quote {
				inString = false
			}
			continue
		}

		// check for comments / strings
		if c == '/' && i+1 < len(content) {
			if content[i+1] == '/' {
				inLineComment = true
				i++
				continue
			}
			if content[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
		}
		if c == '"' || c == '\'' || c == '`' {
			inString = true
			quote = c
			continue
		}

		// check for brace or semicolon
		if c == '{' {
			return i, -1
		}
		if c == ';' {
			return -1, i
		}
	}
	return -1, -1
}

func lineOffsets(content string) []int {
	offsets := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

func lineForOffset(offsets []int, offset int) int {
	lo, hi := 0, len(offsets)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if offsets[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

func buildPythonChunks(filePath, relPath, content string, indexedAt time.Time) []models.Chunk {
	res := codeParser.Parse(models.LangPython, content)
	if len(res.Symbols) == 0 {
		return nil
	}

	lines := logicalLines(content)
	var chunks []models.Chunk

	for _, sym := range res.Symbols {
		if sym.Line <= 0 || sym.Line > len(lines) {
			continue
		}
		baseIndent := indentationLevel(lines[sym.Line-1])
		if baseIndent < 0 {
			baseIndent = 0
		}

		endLine := len(lines)
		for L := sym.Line + 1; L <= len(lines); L++ {
			line := lines[L-1]
			indent := indentationLevel(line)
			if indent >= 0 && indent <= baseIndent {
				endLine = L - 1
				break
			}
		}

		// strip trailing blank/comment lines
		for endLine > sym.Line {
			trimmed := strings.TrimSpace(lines[endLine-1])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				endLine--
			} else {
				break
			}
		}

		chunks = append(chunks, newChunkFromLines(filePath, sym.Kind, sym.Name, sym.Line, endLine, lines, indexedAt))
	}

	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].StartLine != chunks[j].StartLine {
			return chunks[i].StartLine < chunks[j].StartLine
		}
		return chunks[i].ChunkID < chunks[j].ChunkID
	})
	return chunks
}

func indentationLevel(line string) int {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return -1
	}
	return len(line) - len(trimmed)
}
