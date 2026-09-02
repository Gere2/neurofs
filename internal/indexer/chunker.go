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

	"github.com/Gere2/neurofs/internal/contextmap"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/models"
	codeParser "github.com/Gere2/neurofs/internal/parser"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/Gere2/neurofs/internal/tokenbudget"
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
	case models.LangMarkdown:
		chunks = buildMarkdownChunks(filePath, relPath, content, indexedAt)
	}
	if len(chunks) == 0 {
		chunks = []models.Chunk{newChunk(filePath, "file", filepath.Base(relPath), 1, countLogicalLines(content), content, indexedAt)}
	}
	chunks = uniqueChunkIDs(chunks)
	assignChunkParents(chunks)
	return chunks
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

func assignChunkParents(chunks []models.Chunk) {
	parentIDs := make(map[string]string)
	for _, chunk := range chunks {
		switch chunk.Kind {
		case "class", "export_class", "type", "interface",
			"func", "export_func", "const", "export_const", "let", "var":
			if chunk.Symbol != "" {
				if _, exists := parentIDs[chunk.Symbol]; !exists {
					parentIDs[chunk.Symbol] = chunk.ChunkID
				}
			}
		}
	}
	for i := range chunks {
		if !isMemberChunkKind(chunks[i].Kind) {
			continue
		}
		parent, ok := parentSymbol(chunks[i].Symbol)
		if !ok {
			continue
		}
		chunks[i].ParentID = parentIDs[parent]
	}
}

func isMemberChunkKind(kind string) bool {
	return kind == "method" || kind == "get" || kind == "set" || kind == "nested_func"
}

func parentSymbol(symbol string) (string, bool) {
	parent, _, ok := strings.Cut(symbol, ".")
	parent = strings.TrimSpace(parent)
	return parent, ok && parent != ""
}

func persistChunks(ctx context.Context, db *storage.DB, embClient *embeddings.Client, rec models.FileRecord, content string) (int, error) {
	chunks := BuildChunks(rec.Path, rec.RelPath, rec.Lang, content, rec.IndexedAt)
	if err := db.UpsertFileAndChunks(rec, chunks); err != nil {
		return 0, err
	}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.Content) == "" {
			continue
		}
		if _, ok, err := db.GetChunkEmbeddingForProvider(
			chunk.ContentHash,
			embClient.ProviderName(),
			embClient.ModelName(),
		); err != nil {
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
	ranges := make([]jsRange, 0, len(res.Symbols))

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
		ranges = append(ranges, jsRange{sym: sym, start: sym.Line, end: endLine})
	}

	for i := range ranges {
		if !isJSClassChunkKind(ranges[i].sym.Kind) {
			continue
		}
		firstChild := 0
		for j := range ranges {
			if i == j || !isMemberChunkKind(ranges[j].sym.Kind) {
				continue
			}
			parent, ok := parentSymbol(ranges[j].sym.Name)
			if !ok || parent != ranges[i].sym.Name {
				continue
			}
			if ranges[j].start > ranges[i].start && ranges[j].start <= ranges[i].end {
				if firstChild == 0 || ranges[j].start < firstChild {
					firstChild = ranges[j].start
				}
			}
		}
		if firstChild > 0 {
			ranges[i].end = trimJSClassHeaderEnd(lines, ranges[i].start, firstChild-1)
		}
	}

	ranges = append(ranges, nestedJSFunctionRanges(content, lines, offsets, ranges)...)

	var chunks []models.Chunk
	for _, r := range ranges {
		chunks = append(chunks, newChunkFromLines(filePath, r.sym.Kind, r.sym.Name, r.start, r.end, lines, indexedAt))
	}

	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].StartLine != chunks[j].StartLine {
			return chunks[i].StartLine < chunks[j].StartLine
		}
		return chunks[i].ChunkID < chunks[j].ChunkID
	})
	return chunks
}

// Factory-style TS/JS code hides its real API inside one huge function
// body: vuejs/core's baseCreateRenderer spans ~2,100 lines whose inner
// `const mountComponent = (...) => {...}` closures ARE the renderer, yet
// the top-level pass indexes the whole factory as a single 15k-token
// chunk with one symbol. These consts thresholds keep the nested scan
// aimed at that shape: only bodies big enough to hide an API are scanned,
// and only the shallowest closure layer is extracted — closures nested
// inside closures are implementation detail, not API surface.
const nestedJSScanMinLines = 40
const nestedJSMaxPerParent = 80

var nestedJSAssignedFuncRe = regexp.MustCompile(`^(\s+)(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)(?:\s*:\s*[^=]+?)?\s*=\s*(?:async\s+)?(?:function\b|\()`)
var nestedJSNamedFuncRe = regexp.MustCompile(`^(\s+)(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)

type jsRange struct {
	sym   models.Symbol
	start int
	end   int
}

type nestedJSMatch struct {
	name   string
	line   int
	indent int
}

// nestedJSFunctionRanges emits child ranges for function-expression
// assignments and named functions nested inside large function bodies.
// Symbols are dotted (`baseCreateRenderer.mountComponent`) so parent
// assignment and `symbol_exact` matching on the last component both work.
// Each nested match is claimed only by its innermost containing function:
// heuristic decl-end detection can overshoot a top-level `let`'s range far
// enough to swallow later functions, and without the innermost rule every
// such bogus parent re-emits the same closure as a duplicate chunk.
func nestedJSFunctionRanges(content string, lines []string, offsets []int, ranges []jsRange) []jsRange {
	topLevel := make(map[string]bool, len(ranges))
	for _, r := range ranges {
		topLevel[r.sym.Name] = true
	}

	type claim struct {
		parent jsRange
		match  nestedJSMatch
	}
	claims := make(map[int]claim) // match line -> innermost parent's claim

	for _, parent := range ranges {
		if parent.end-parent.start < nestedJSScanMinLines {
			continue
		}
		if !isJSFunctionRange(parent, lines) {
			continue
		}

		var matches []nestedJSMatch
		minIndent := 0
		for lineNo := parent.start + 1; lineNo < parent.end && lineNo <= len(lines); lineNo++ {
			line := lines[lineNo-1]
			m := nestedJSAssignedFuncRe.FindStringSubmatch(line)
			if m == nil {
				m = nestedJSNamedFuncRe.FindStringSubmatch(line)
			}
			if m == nil {
				continue
			}
			name := m[2]
			if name == parent.sym.Name || topLevel[name] {
				continue
			}
			indent := indentationLevel(line)
			matches = append(matches, nestedJSMatch{name: name, line: lineNo, indent: indent})
			if minIndent == 0 || indent < minIndent {
				minIndent = indent
			}
		}

		emitted := 0
		for _, match := range matches {
			if match.indent != minIndent {
				continue
			}
			if emitted >= nestedJSMaxPerParent {
				break
			}
			if prev, taken := claims[match.line]; taken {
				if parent.end-parent.start >= prev.parent.end-prev.parent.start {
					continue
				}
			}
			claims[match.line] = claim{parent: parent, match: match}
			emitted++
		}
	}

	var out []jsRange
	for _, c := range claims {
		startOffset := offsets[c.match.line-1]
		braceOffset, semiOffset := findJSDeclEnd(content, startOffset)
		endLine := c.match.line
		if braceOffset >= 0 {
			if _, bodyEnd := codeParser.FindBraceBody(content, braceOffset); bodyEnd >= 0 {
				endLine = lineForOffset(offsets, bodyEnd)
			}
		} else if semiOffset >= 0 {
			endLine = lineForOffset(offsets, semiOffset)
		}
		if endLine < c.match.line {
			endLine = c.match.line
		}
		if endLine > c.parent.end {
			endLine = c.parent.end
		}
		out = append(out, jsRange{
			sym: models.Symbol{
				Name: c.parent.sym.Name + "." + c.match.name,
				Kind: "nested_func",
				Line: c.match.line,
			},
			start: c.match.line,
			end:   endLine,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// isJSFunctionRange reports whether a top-level range is actually a
// function body worth scanning: named functions by kind, and const/let/var
// declarations only when their declaration line shows a function value —
// a plain `let state = {...}` object can span hundreds of lines and its
// contents are not API closures.
func isJSFunctionRange(r jsRange, lines []string) bool {
	switch r.sym.Kind {
	case "func", "export_func":
		return true
	case "const", "export_const", "let", "var":
		if r.start >= 1 && r.start <= len(lines) {
			decl := lines[r.start-1]
			return strings.Contains(decl, "=>") || strings.Contains(decl, "function")
		}
	}
	return false
}

func isJSClassChunkKind(kind string) bool {
	return kind == "class" || kind == "export_class"
}

func trimJSClassHeaderEnd(lines []string, startLine, endLine int) int {
	for endLine > startLine {
		trimmed := strings.TrimSpace(lines[endLine-1])
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "/*") ||
			strings.HasPrefix(trimmed, "*") ||
			strings.HasPrefix(trimmed, "@") {
			endLine--
			continue
		}
		break
	}
	return endLine
}

func findJSDeclEnd(content string, startOffset int) (braceOffset int, semiOffset int) {
	inString := false
	var quote byte
	inLineComment := false
	inBlockComment := false
	parenDepth := 0
	bracketDepth := 0
	var lastSignificant byte

	for i := startOffset; i < len(content); i++ {
		c := content[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
				if parenDepth == 0 && bracketDepth == 0 &&
					!jsDeclarationContinuesAfter(lastSignificant) &&
					nextJSDeclarationStart(content, i+1) >= 0 {
					return -1, i
				}
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
			lastSignificant = c
			continue
		}

		switch c {
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '{':
			// Object literals and destructuring patterns in a parameter list
			// are part of the signature, not the declaration's body.
			if parenDepth == 0 && bracketDepth == 0 {
				return i, -1
			}
		case ';':
			if parenDepth == 0 && bracketDepth == 0 {
				return -1, i
			}
		case '\n':
			// TypeScript commonly omits semicolons. Stop an unbraced
			// declaration before the next top-level declaration instead of
			// borrowing that declaration's first body brace.
			if parenDepth == 0 && bracketDepth == 0 &&
				!jsDeclarationContinuesAfter(lastSignificant) &&
				nextJSDeclarationStart(content, i+1) >= 0 {
				return -1, i
			}
		default:
			if !isJSSpace(c) {
				lastSignificant = c
			}
		}
	}
	return -1, -1
}

func jsDeclarationContinuesAfter(last byte) bool {
	switch last {
	case '=', '>', ',', '.', '(', '[', '|', '&', '?', ':', '+', '-', '*', '/', '%', '!':
		return true
	default:
		return false
	}
}

func nextJSDeclarationStart(content string, offset int) int {
	for offset < len(content) {
		for offset < len(content) && isJSSpace(content[offset]) {
			offset++
		}
		if offset >= len(content) {
			return -1
		}
		if offset+1 < len(content) && content[offset] == '/' {
			switch content[offset+1] {
			case '/':
				offset += 2
				for offset < len(content) && content[offset] != '\n' {
					offset++
				}
				continue
			case '*':
				end := strings.Index(content[offset+2:], "*/")
				if end < 0 {
					return -1
				}
				offset += end + 4
				continue
			}
		}
		break
	}

	if offset >= len(content) {
		return -1
	}
	rest := content[offset:]
	if hasJSWordPrefix(rest, "export") ||
		hasJSWordPrefix(rest, "import") ||
		hasJSWordPrefix(rest, "function") ||
		hasJSWordPrefix(rest, "class") ||
		hasJSWordPrefix(rest, "interface") ||
		hasJSWordPrefix(rest, "type") ||
		hasJSWordPrefix(rest, "enum") ||
		hasJSWordPrefix(rest, "namespace") ||
		hasJSWordPrefix(rest, "module") ||
		hasJSWordPrefix(rest, "const") ||
		hasJSWordPrefix(rest, "let") ||
		hasJSWordPrefix(rest, "var") ||
		hasJSWordPrefix(rest, "declare") ||
		hasJSWordPrefix(rest, "abstract") ||
		hasJSWordPrefix(rest, "async") {
		return offset
	}
	return -1
}

func hasJSWordPrefix(s, word string) bool {
	if !strings.HasPrefix(s, word) {
		return false
	}
	if len(s) == len(word) {
		return true
	}
	next := s[len(word)]
	switch {
	case next >= 'a' && next <= 'z',
		next >= 'A' && next <= 'Z',
		next >= '0' && next <= '9',
		next == '_',
		next == '$':
		return false
	default:
		return true
	}
}

func isJSSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
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

// buildMarkdownChunks splits a document at its h1–h3 headings (the levels
// the parser already extracts as symbols): section = heading line through
// the line before the next heading, heading text = symbol. Before this,
// every .md was one whole-file chunk (2.3k tokens average in this repo) —
// no symbol to match, long-chunk-penalized, so doc-content queries lost to
// code files stacking structural boosts. Measured origin: a real query for
// cross-shape results missed docs/phase_g5_cross_shape.md entirely
// (learned fixture 680b59f7aed5). Headingless documents keep the
// whole-file fallback.
func buildMarkdownChunks(filePath, relPath, content string, indexedAt time.Time) []models.Chunk {
	res := codeParser.Parse(models.LangMarkdown, content)
	if len(res.Symbols) == 0 {
		return nil
	}

	lines := logicalLines(content)
	total := len(lines)
	var chunks []models.Chunk

	if first := res.Symbols[0].Line; first > 1 {
		chunks = append(chunks, newChunkFromLines(filePath, "section", "preamble", 1, first-1, lines, indexedAt))
	}
	// ancestors[level-1] holds the heading currently open at that level, so a
	// section knows the chain it hangs off: "Roadmap/Sprint S6/S6.1".
	var ancestors [maxMarkdownHeadingLevel]string
	for i, sym := range res.Symbols {
		if sym.Line <= 0 || sym.Line > total {
			continue
		}
		level := markdownHeadingLevel(sym.Kind)
		ancestors[level-1] = sym.Name
		for deeper := level; deeper < maxMarkdownHeadingLevel; deeper++ {
			ancestors[deeper] = ""
		}
		end := total
		if i+1 < len(res.Symbols) {
			end = res.Symbols[i+1].Line - 1
		}
		if end < sym.Line {
			end = sym.Line
		}
		chunk := newChunkFromLines(filePath, "section", sym.Name, sym.Line, end, lines, indexedAt)
		chunk.HeadingPath = joinHeadingPath(ancestors[:level])
		chunks = append(chunks, chunk)
	}
	return chunks
}

// maxMarkdownHeadingLevel matches the h1–h3 range the parser extracts.
const maxMarkdownHeadingLevel = 3

// markdownHeadingLevel maps the parser's "h1".."h3" symbol kind back to its
// depth. Anything unexpected is treated as a top-level heading rather than
// dropped, so a section always gets a heading path.
func markdownHeadingLevel(kind string) int {
	if len(kind) == 2 && kind[0] == 'h' {
		if level := int(kind[1] - '0'); level >= 1 && level <= maxMarkdownHeadingLevel {
			return level
		}
	}
	return 1
}

// joinHeadingPath slash-joins the open headings, skipping levels a document
// never opened (a file that starts at "##" has no h1 to inherit).
func joinHeadingPath(ancestors []string) string {
	segments := make([]string, 0, len(ancestors))
	for _, heading := range ancestors {
		heading = strings.TrimSpace(heading)
		if heading == "" {
			continue
		}
		segments = append(segments, heading)
	}
	return strings.Join(segments, "/")
}

func buildPythonChunks(filePath, relPath, content string, indexedAt time.Time) []models.Chunk {
	res := codeParser.Parse(models.LangPython, content)
	if len(res.Symbols) == 0 {
		return nil
	}

	lines := logicalLines(content)

	// First pass: indentation-scoped range per symbol. The parser emits
	// methods as their own symbols (Class.method), so ranges nest: a class
	// range covers its whole body, each method covers its own block.
	type symRange struct {
		sym   models.Symbol
		start int
		end   int
	}
	ranges := make([]symRange, 0, len(res.Symbols))
	for _, sym := range res.Symbols {
		if sym.Line <= 0 || sym.Line > len(lines) {
			continue
		}
		baseIndent := indentationLevel(lines[sym.Line-1])
		if baseIndent < 0 {
			baseIndent = 0
		}

		// A Python signature can span multiple lines, with the closing
		// parenthesis back at the declaration's indentation. Do not mistake
		// that closing line for the next declaration: first locate the
		// top-level colon that opens the suite, then apply indentation rules
		// only to the body that follows.
		headerEnd := pythonHeaderEndLine(lines, sym.Line)
		endLine := len(lines)
		for L := headerEnd + 1; L <= len(lines); L++ {
			line := lines[L-1]
			indent := indentationLevel(line)
			if indent >= 0 && indent <= baseIndent {
				endLine = L - 1
				break
			}
		}
		ranges = append(ranges, symRange{sym: sym, start: sym.Line, end: endLine})
	}

	// Second pass: cap each class at its first child symbol so the class
	// chunk carries the header — class line, docstring, class-level
	// attributes — instead of duplicating every method body. Without the cap
	// a large class (click's Context is ~1000 lines) becomes one chunk that
	// is too big to be cheap and too blunt to target.
	for i := range ranges {
		if ranges[i].sym.Kind != "class" {
			continue
		}
		for j := range ranges {
			if j == i {
				continue
			}
			if ranges[j].start > ranges[i].start && ranges[j].start <= ranges[i].end {
				if headerEnd := ranges[j].start - 1; headerEnd < ranges[i].end {
					ranges[i].end = headerEnd
				}
			}
		}
	}

	var chunks []models.Chunk
	for _, r := range ranges {
		endLine := r.end
		// strip trailing blank/comment/decorator lines (a decorator above the
		// first method belongs to the method, not the class header)
		for endLine > r.start {
			trimmed := strings.TrimSpace(lines[endLine-1])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "@") {
				endLine--
			} else {
				break
			}
		}
		chunks = append(chunks, newChunkFromLines(filePath, r.sym.Kind, r.sym.Name, r.start, endLine, lines, indexedAt))
	}

	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].StartLine != chunks[j].StartLine {
			return chunks[i].StartLine < chunks[j].StartLine
		}
		return chunks[i].ChunkID < chunks[j].ChunkID
	})
	return chunks
}

func pythonHeaderEndLine(lines []string, startLine int) int {
	if startLine < 1 || startLine > len(lines) {
		return startLine
	}
	depth := 0
	var quote rune
	escaped := false
	for lineNo := startLine; lineNo <= len(lines); lineNo++ {
		for _, r := range lines[lineNo-1] {
			if escaped {
				escaped = false
				continue
			}
			if quote != 0 {
				if r == '\\' {
					escaped = true
					continue
				}
				if r == quote {
					quote = 0
				}
				continue
			}
			switch r {
			case '\'', '"':
				quote = r
			case '#':
				goto nextLine
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			case ':':
				if depth == 0 {
					return lineNo
				}
			}
		}
	nextLine:
	}
	return startLine
}

func indentationLevel(line string) int {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return -1
	}
	return len(line) - len(trimmed)
}
