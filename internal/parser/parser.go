// Package parser extracts symbols and imports from source files.
// It uses heuristic regex-based extraction — precise enough for ranking,
// no AST parser required in the MVP.
package parser

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// Result is what the parser extracts from a file.
type Result struct {
	Symbols   []models.Symbol
	Imports   []string
	Signature string // compact, human-readable representation of the file's interface
}

// Parse extracts symbols and imports from the given content for the given language.
func Parse(lang models.Lang, content string) Result {
	switch lang {
	case models.LangTypeScript, models.LangJavaScript:
		return parseJS(content)
	case models.LangPython:
		return parsePython(content)
	case models.LangGo:
		return parseGo(content)
	case models.LangMarkdown:
		return parseMarkdown(content)
	case models.LangJSON, models.LangYAML:
		return parseStructured(lang, content)
	default:
		return Result{}
	}
}

// ─── TypeScript / JavaScript ─────────────────────────────────────────────────

var (
	reJSImport      = regexp.MustCompile(`(?m)^import\s+.+from\s+['"]([^'"]+)['"]`)
	reJSRequire     = regexp.MustCompile(`require\(['"]([^'"]+)['"]\)`)
	reJSExportFn    = regexp.MustCompile(`(?m)^export\s+(?:async\s+)?function\s+(\w+)`)
	reJSExportClass = regexp.MustCompile(`(?m)^export\s+(?:default\s+)?class\s+(\w+)`)
	reJSExportConst = regexp.MustCompile(`(?m)^export\s+(?:const|let|var)\s+(\w+)`)
	reJSExportType  = regexp.MustCompile(`(?m)^export\s+(?:type|interface)\s+(\w+)`)
	reJSExportDef   = regexp.MustCompile(`(?m)^export\s+default\s+(?:function\s+)?(\w+)`)
	reJSFn          = regexp.MustCompile(`(?m)^(?:async\s+)?function\s+(\w+)`)
	reJSClass       = regexp.MustCompile(`(?m)^class\s+(\w+)`)

	// Any class declaration, exported or not, used for method extraction.
	reJSClassAny = regexp.MustCompile(`(?m)^(?:export\s+(?:default\s+)?)?(?:abstract\s+)?class\s+(\w+)`)

	// Class-body member patterns. All assume we're already positioned at a
	// line that lives at the class-body level (depth 0 within the body).
	reTSMethod   = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|async|readonly|override|abstract)\s+)*(\w+)\s*(?:<[^>]*>)?\s*\(`)
	reTSArrow    = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|readonly|override)\s+)*(\w+)\s*=\s*(?:async\s+)?(?:\([^)]*\)|\w+)\s*(?::[^=]+)?=>`)
	reTSAccessor = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static)\s+)*(get|set)\s+(\w+)\s*\(`)
)

// classMemberBlacklist filters out control-flow and reserved words that the
// method regexes would otherwise match when a line happens to start with
// them. Actual class members never use these names.
var classMemberBlacklist = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"throw": true, "typeof": true, "new": true, "catch": true, "else": true,
	"do": true, "try": true, "function": true, "class": true, "else if": true,
	"await": true, "yield": true, "this": true, "super": true, "import": true,
}

func parseJS(content string) Result {
	var r Result

	for _, m := range reJSImport.FindAllStringSubmatch(content, -1) {
		r.Imports = append(r.Imports, m[1])
	}
	for _, m := range reJSRequire.FindAllStringSubmatch(content, -1) {
		r.Imports = append(r.Imports, m[1])
	}
	r.Imports = unique(r.Imports)

	lines := lineIndex(content)

	addSymbols := func(re *regexp.Regexp, kind string) {
		for _, m := range re.FindAllStringSubmatchIndex(content, -1) {
			r.Symbols = append(r.Symbols, models.Symbol{
				Name: content[m[2]:m[3]],
				Kind: kind,
				Line: lineForOffset(lines, m[0]),
			})
		}
	}

	addSymbols(reJSExportFn, "export_func")
	addSymbols(reJSExportClass, "export_class")
	addSymbols(reJSExportConst, "export_const")
	addSymbols(reJSExportType, "export_type")
	addSymbols(reJSExportDef, "export_default")
	addSymbols(reJSFn, "func")
	addSymbols(reJSClass, "class")

	// Capture class methods (traditional, arrow-field, get/set). Qualified as
	// "ClassName.methodName" so queries like "authenticate" still match and
	// readers of the signature see where the method lives.
	r.Symbols = append(r.Symbols, extractClassMethods(content)...)

	r.Symbols = deduplicateSymbols(r.Symbols)

	var sb strings.Builder
	for _, imp := range r.Imports {
		fmt.Fprintf(&sb, "// import: %s\n", imp)
	}
	for _, s := range r.Symbols {
		switch s.Kind {
		case "export_func":
			fmt.Fprintf(&sb, "export function %s(...) { ... }\n", s.Name)
		case "export_class":
			fmt.Fprintf(&sb, "export class %s { ... }\n", s.Name)
		case "export_const":
			fmt.Fprintf(&sb, "export const %s = ...\n", s.Name)
		case "export_type":
			fmt.Fprintf(&sb, "export type/interface %s { ... }\n", s.Name)
		case "export_default":
			fmt.Fprintf(&sb, "export default %s\n", s.Name)
		case "func":
			fmt.Fprintf(&sb, "function %s(...) { ... }\n", s.Name)
		case "class":
			fmt.Fprintf(&sb, "class %s { ... }\n", s.Name)
		case "method":
			fmt.Fprintf(&sb, "  %s(...): ...\n", s.Name)
		case "get":
			fmt.Fprintf(&sb, "  get %s(): ...\n", s.Name)
		case "set":
			fmt.Fprintf(&sb, "  set %s(...): void\n", s.Name)
		}
	}
	r.Signature = sb.String()
	return r
}

// extractClassMethods finds every `class X` block in content and returns the
// methods declared in its body, each named "X.methodName". Method extraction
// is heuristic: it tracks brace depth so we only look at members at the
// class-body level, and filters out control-flow keywords that would
// otherwise regex-match like methods.
func extractClassMethods(content string) []models.Symbol {
	var syms []models.Symbol
	lines := lineIndex(content)
	seen := make(map[string]bool)

	for _, m := range reJSClassAny.FindAllStringSubmatchIndex(content, -1) {
		className := content[m[2]:m[3]]
		bodyStart, bodyEnd := findBraceBody(content, m[1])
		if bodyStart < 0 {
			continue
		}
		body := content[bodyStart:bodyEnd]
		for _, sym := range methodsInBody(body, className, bodyStart, lines, seen) {
			syms = append(syms, sym)
		}
	}
	return syms
}

// findBraceBody locates the first `{` at or after startIdx, then returns the
// byte range (exclusive of the outer braces) up to its matching `}`. It
// handles nested braces, strings, and // and /* */ comments so that braces
// appearing inside those regions don't fool the counter.
//
// Returns (-1, -1) when no balanced body is found.
func findBraceBody(content string, startIdx int) (int, int) {
	openIdx := -1
	for i := startIdx; i < len(content); i++ {
		c := content[i]
		if c == '{' {
			openIdx = i
			break
		}
		if c == ';' || c == '\n' && openIdx == -1 && looksLikeClassHeaderEnd(content, i) {
			// abstract class with no body, or a class reference, not a decl.
			return -1, -1
		}
	}
	if openIdx == -1 {
		return -1, -1
	}
	depth := 1
	i := openIdx + 1
	for i < len(content) {
		c := content[i]
		switch c {
		case '"', '\'', '`':
			i = skipString(content, i, c)
			continue
		case '/':
			if i+1 < len(content) {
				if content[i+1] == '/' {
					for i < len(content) && content[i] != '\n' {
						i++
					}
					continue
				}
				if content[i+1] == '*' {
					i += 2
					for i+1 < len(content) && !(content[i] == '*' && content[i+1] == '/') {
						i++
					}
					i += 2
					continue
				}
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return openIdx + 1, i
			}
		}
		i++
	}
	return -1, -1
}

// looksLikeClassHeaderEnd returns true when a newline at offset i in content
// precedes characters that look like a class reference rather than a class
// declaration body (e.g. `class Foo extends Bar;`). Called only when we've
// seen no opening brace yet.
func looksLikeClassHeaderEnd(_ string, _ int) bool {
	// Placeholder — we currently treat any unbraced class declaration as a
	// no-body class and skip it. Kept as a named hook for readability.
	return false
}

// skipString advances past a string literal starting at i (whose quote is q),
// honouring escape sequences. Returns the index of the character after the
// closing quote, or len(content) if unterminated.
func skipString(content string, i int, q byte) int {
	i++
	for i < len(content) {
		c := content[i]
		if c == '\\' && i+1 < len(content) {
			i += 2
			continue
		}
		if c == q {
			return i + 1
		}
		i++
	}
	return i
}

// methodsInBody walks the body line by line, tracking brace depth so we only
// consider member declarations at the class-body level. bodyOffset is the
// byte offset of body[0] inside the full file; used to compute line numbers.
// The seen map is keyed "ClassName.name|kind" so getters and setters of the
// same property are both captured.
func methodsInBody(body, className string, bodyOffset int, fileLines []int, seen map[string]bool) []models.Symbol {
	var syms []models.Symbol

	// Iterate by line, tracking absolute offset for line-number computation.
	depth := 0
	i := 0
	for i < len(body) {
		// Find the end of the current line.
		lineStart := i
		for i < len(body) && body[i] != '\n' {
			i++
		}
		line := body[lineStart:i]
		if i < len(body) {
			i++ // consume '\n'
		}

		lineDepth := depth
		// Update depth from the line's braces (naive; strings/comments on
		// the same line may briefly confuse it, but members we care about
		// sit at depth 0 which is robust in practice).
		for _, c := range line {
			switch c {
			case '{':
				depth++
			case '}':
				depth--
			}
		}

		if lineDepth != 0 {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "*") ||
			strings.HasPrefix(trimmed, "/*") {
			continue
		}

		name, kind := "", "method"
		if m := reTSAccessor.FindStringSubmatch(line); m != nil {
			kind = m[1]
			name = m[2]
		} else if m := reTSArrow.FindStringSubmatch(line); m != nil {
			name = m[1]
		} else if m := reTSMethod.FindStringSubmatch(line); m != nil {
			name = m[1]
		}
		if name == "" || classMemberBlacklist[name] {
			continue
		}
		qualified := className + "." + name
		dedupKey := qualified + "|" + kind
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true

		syms = append(syms, models.Symbol{
			Name: qualified,
			Kind: kind,
			Line: lineForOffset(fileLines, bodyOffset+lineStart),
		})
	}
	return syms
}

// ─── Python ──────────────────────────────────────────────────────────────────

var (
	rePyImport     = regexp.MustCompile(`(?m)^import\s+(\S+)`)
	rePyFromImport = regexp.MustCompile(`(?m)^from\s+(\S+)\s+import`)
	rePyDef        = regexp.MustCompile(`(?m)^(?:async\s+)?def\s+(\w+)`)
	rePyClass      = regexp.MustCompile(`(?m)^class\s+(\w+)`)
)

func parsePython(content string) Result {
	var r Result

	for _, m := range rePyImport.FindAllStringSubmatch(content, -1) {
		r.Imports = append(r.Imports, m[1])
	}
	for _, m := range rePyFromImport.FindAllStringSubmatch(content, -1) {
		r.Imports = append(r.Imports, m[1])
	}
	r.Imports = unique(r.Imports)

	lines := lineIndex(content)

	for _, m := range rePyDef.FindAllStringSubmatchIndex(content, -1) {
		r.Symbols = append(r.Symbols, models.Symbol{
			Name: content[m[2]:m[3]],
			Kind: "func",
			Line: lineForOffset(lines, m[0]),
		})
	}
	for _, m := range rePyClass.FindAllStringSubmatchIndex(content, -1) {
		r.Symbols = append(r.Symbols, models.Symbol{
			Name: content[m[2]:m[3]],
			Kind: "class",
			Line: lineForOffset(lines, m[0]),
		})
	}

	var sb strings.Builder
	for _, imp := range r.Imports {
		fmt.Fprintf(&sb, "# import: %s\n", imp)
	}
	for _, s := range r.Symbols {
		switch s.Kind {
		case "func":
			fmt.Fprintf(&sb, "def %s(...): ...\n", s.Name)
		case "class":
			fmt.Fprintf(&sb, "class %s: ...\n", s.Name)
		}
	}
	r.Signature = sb.String()
	return r
}

// ─── Go ──────────────────────────────────────────────────────────────────────

var (
	reGoImportSingle = regexp.MustCompile(`(?m)^import\s+"([^"]+)"`)
	reGoImportGroup  = regexp.MustCompile(`"([^"]+)"`)
	reGoImportBlock  = regexp.MustCompile(`(?ms)import\s+\((.+?)\)`)
	reGoFunc         = regexp.MustCompile(`(?m)^func\s+(?:\(\w+\s+\*?\w+\)\s+)?(\w+)\(`)
	reGoType         = regexp.MustCompile(`(?m)^type\s+(\w+)\s+`)
	reGoConst        = regexp.MustCompile(`(?m)^const\s+(\w+)\s`)
	reGoVar          = regexp.MustCompile(`(?m)^var\s+(\w+)\s`)

	// Parenthesised const/var blocks. These were the blind spot that
	// hid every grouped const from the index — models.go's RepFullCode /
	// RepExcerpt / RepSignature, packager.go's fullCodeMaxTokens etc.
	// The single-line regexes above don't fire on `const Foo = ...`
	// when Foo lives inside a `const ( ... )` block (the `^const` at
	// the spec's indentation is missing). The block regexes match the
	// outer wrapper and then per-line extraction inside the body
	// recovers each spec name. The body capture is non-greedy and
	// terminates at a `)` at start of line — bodies cannot contain a
	// `)` at column 0 in well-formed Go.
	reGoConstBlock = regexp.MustCompile(`(?ms)^const\s*\(\s*\n(.*?)^\)`)
	reGoVarBlock   = regexp.MustCompile(`(?ms)^var\s*\(\s*\n(.*?)^\)`)
	// reGoBlockSpecName captures the leading identifier (or comma-
	// separated list of identifiers) on a spec line. Comments, blank
	// lines, and continuation lines that do not start with an
	// identifier all fail to match and are skipped.
	reGoBlockSpecName = regexp.MustCompile(`^\s*(\w+(?:\s*,\s*\w+)*)`)
)

func parseGo(content string) Result {
	var r Result

	for _, m := range reGoImportSingle.FindAllStringSubmatch(content, -1) {
		r.Imports = append(r.Imports, m[1])
	}
	for _, block := range reGoImportBlock.FindAllStringSubmatch(content, -1) {
		for _, m := range reGoImportGroup.FindAllStringSubmatch(block[1], -1) {
			r.Imports = append(r.Imports, m[1])
		}
	}
	r.Imports = unique(r.Imports)

	lines := lineIndex(content)

	addSym := func(re *regexp.Regexp, kind string) {
		for _, m := range re.FindAllStringSubmatchIndex(content, -1) {
			r.Symbols = append(r.Symbols, models.Symbol{
				Name: content[m[2]:m[3]],
				Kind: kind,
				Line: lineForOffset(lines, m[0]),
			})
		}
	}

	addSym(reGoFunc, "func")
	addSym(reGoType, "type")
	addSym(reGoConst, "const")
	addSym(reGoVar, "var")

	// Recover specs hidden inside parenthesised const/var blocks. The
	// single-line addSym calls above naturally skip these because the
	// inner specs do not start with the `const`/`var` keyword.
	r.Symbols = append(r.Symbols, extractGoBlockSpecs(content, lines, "const", reGoConstBlock)...)
	r.Symbols = append(r.Symbols, extractGoBlockSpecs(content, lines, "var", reGoVarBlock)...)
	r.Symbols = deduplicateSymbols(r.Symbols)

	var sb strings.Builder
	for _, imp := range r.Imports {
		fmt.Fprintf(&sb, "// import: %s\n", imp)
	}
	for _, s := range r.Symbols {
		switch s.Kind {
		case "func":
			fmt.Fprintf(&sb, "func %s(...) { ... }\n", s.Name)
		case "type":
			fmt.Fprintf(&sb, "type %s struct/interface { ... }\n", s.Name)
		case "const":
			fmt.Fprintf(&sb, "const %s = ...\n", s.Name)
		case "var":
			fmt.Fprintf(&sb, "var %s ...\n", s.Name)
		}
	}
	r.Signature = sb.String()
	return r
}

// extractGoBlockSpecs scans content for parenthesised const/var blocks
// matched by blockRe and returns one Symbol per declared name. Spec
// lines like `Foo Lang = "go"` yield Symbol{Name: "Foo"}; lines like
// `a, b = 1, 2` yield two Symbols. Lines that begin with `/`, `*`, a
// digit, or whitespace-then-a-non-identifier are skipped, which
// handles comments and blank lines without an explicit branch.
//
// Line numbers are computed from the global lineStarts so reported
// positions match the source file, not an offset within the body.
func extractGoBlockSpecs(content string, lineStarts []int, kind string, blockRe *regexp.Regexp) []models.Symbol {
	var syms []models.Symbol
	for _, m := range blockRe.FindAllStringSubmatchIndex(content, -1) {
		// m[2:4] is the body capture group (inside the parens, exclusive
		// of the closing `)` line).
		bodyStart, bodyEnd := m[2], m[3]
		if bodyStart < 0 {
			continue
		}
		offset := bodyStart
		for _, line := range strings.SplitAfter(content[bodyStart:bodyEnd], "\n") {
			match := reGoBlockSpecName.FindStringSubmatch(line)
			if match != nil {
				lineNum := lineForOffset(lineStarts, offset)
				for _, n := range strings.Split(match[1], ",") {
					n = strings.TrimSpace(n)
					if n == "" || n == "_" {
						continue
					}
					syms = append(syms, models.Symbol{
						Name: n, Kind: kind, Line: lineNum,
					})
				}
			}
			offset += len(line)
		}
	}
	return syms
}

// ─── Markdown ────────────────────────────────────────────────────────────────

var reMarkdownH = regexp.MustCompile(`(?m)^(#{1,3})\s+(.+)`)

func parseMarkdown(content string) Result {
	var r Result
	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		text := scanner.Text()
		if m := reMarkdownH.FindStringSubmatch(text); m != nil {
			level := len(m[1])
			r.Symbols = append(r.Symbols, models.Symbol{
				Name: strings.TrimSpace(m[2]),
				Kind: fmt.Sprintf("h%d", level),
				Line: lineNum,
			})
		}
	}

	var sb strings.Builder
	for _, s := range r.Symbols {
		fmt.Fprintf(&sb, "%s: %s\n", s.Kind, s.Name)
	}
	r.Signature = sb.String()
	return r
}

// ─── JSON / YAML ─────────────────────────────────────────────────────────────

func parseStructured(lang models.Lang, content string) Result {
	lines := strings.Count(content, "\n") + 1
	return Result{
		Signature: fmt.Sprintf("// %s file, %d lines\n", string(lang), lines),
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// lineIndex returns byte offsets of each line start.
func lineIndex(content string) []int {
	offsets := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' && i+1 < len(content) {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

// lineForOffset returns the 1-based line number for a byte offset.
func lineForOffset(lineStarts []int, offset int) int {
	lo, hi := 0, len(lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lineStarts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

// unique deduplicates a string slice, preserving order.
func unique(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// deduplicateSymbols removes symbols with duplicate (name, kind) pairs.
// Getters and setters of the same name are preserved because they differ
// in kind.
func deduplicateSymbols(syms []models.Symbol) []models.Symbol {
	type key struct{ name, kind string }
	seen := make(map[key]bool, len(syms))
	out := syms[:0]
	for _, s := range syms {
		k := key{s.Name, s.Kind}
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}
