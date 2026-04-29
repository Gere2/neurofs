// Sub-file extraction for the top-ranked files in a bundle.
//
// The packager's default chain is full_code → signature → structural_note,
// which is all-or-nothing per file. For a 1500-token TS module the choice
// becomes "throw away every body" (signature) vs. "spend the whole budget on
// one file" (full_code). Excerpt fills the gap: identify which symbols in the
// file the query is asking about, emit just those bodies, elide the rest with
// `// ... N lines omitted ...` markers.
//
// Scope is intentionally narrow:
//   - TypeScript / JavaScript / Python only (Go later — go/ast would be the
//     natural upgrade and is a different change)
//   - Heuristic block boundaries (brace counting for JS/TS, indent for
//     Python). No AST. Wrong extraction degrades to a wider line range, never
//     to broken code.
//   - Caller decides which files are eligible (top N ranked by position).
package packager

import (
	"fmt"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/ranking"
)

// excerptTopN caps how many of the top-ranked files in a Pack call may be
// considered for sub-file extraction. The constraint exists for two reasons:
// extraction is more expensive per file than signature, and excerpts are
// verbose enough that more than three would crowd out coverage of the rest
// of the bundle.
const excerptTopN = 3

// excerptMaxTokens caps a single excerpt fragment. Sized between
// signatureMaxTokens (350) and fullCodeMaxTokens (600 default) — past 800 we
// would be better off either including the whole file (if budget allows) or
// falling back to a signature (if it does not).
const excerptMaxTokens = 800

// excerptBlockMaxLines caps a single rendered block at this many source
// lines. Bigger blocks are truncated to {excerptBlockHeadLines} + an
// elision marker + {excerptBlockTailLines} so that one oversized
// function (e.g. a 150-line audit-replay handler) does not push the
// whole excerpt past excerptMaxTokens and force a fallback to signature.
// 40 is the empirical landing zone: it preserves the function signature,
// the first lines of logic, and the closing return/braces, while
// shrinking 100+ line bodies enough to fit alongside neighbour blocks.
const excerptBlockMaxLines = 40

// excerptBlockHeadLines and excerptBlockTailLines control how many
// lines are kept on each side of an elision in a truncated block. The
// head needs enough room for the function signature, doc comment, and
// the first one or two statements of the body; the tail captures the
// final return and closing brace. Their sum sits well below
// excerptBlockMaxLines so truncation always shrinks.
const (
	excerptBlockHeadLines = 8
	excerptBlockTailLines = 4
)

// block is a closed line range [startLine, endLine] inside a file, tagged
// with the symbol name(s) that motivated its inclusion.
type block struct {
	startLine int // 1-based, inclusive
	endLine   int // 1-based, inclusive
	symbol    string
}

// isExcerptLang reports whether sub-file extraction is supported for lang.
// Each supported language has its own walker matched to its actual scoping
// rules — no language gets a generic fallback because broken excerpts
// would be worse than the signature representation we already have.
func isExcerptLang(lang models.Lang) bool {
	switch lang {
	case models.LangTypeScript, models.LangJavaScript, models.LangPython, models.LangGo:
		return true
	}
	return false
}

// extractExcerpt builds a sub-file excerpt for rec containing the symbol
// blocks whose names lexically match any of terms. Returns ok=false when
// no useful excerpt could be assembled — caller falls back to signature.
//
// The output is self-contained: a header with file/lang/representation,
// then alternating `// ── path:start-end (symbol) ──` markers and code
// blocks, with `// ... N lines omitted ...` separating them. The model
// reading the prompt always knows which lines it has and which it does
// not.
func extractExcerpt(rec models.FileRecord, content string, terms []string) (string, bool) {
	if len(terms) == 0 || strings.TrimSpace(content) == "" {
		return "", false
	}
	if !isExcerptLang(rec.Lang) {
		return "", false
	}

	// Go has its own AST-based path because go/parser gives us exact line
	// ranges AND lets us discover struct field names that the regex
	// parser does not expose as symbols. Without this branch, queries
	// like "UpgradeWithSlack" or "PreferSignatures" miss every Go file
	// because rec.Symbols never contains struct field names.
	if rec.Lang == models.LangGo {
		return extractGoExcerpt(rec, content, terms)
	}

	if len(rec.Symbols) == 0 {
		return "", false
	}
	matched := matchingSymbols(rec.Symbols, terms)
	if len(matched) == 0 {
		return "", false
	}

	lines := strings.Split(content, "\n")
	blocks := make([]block, 0, len(matched))
	for _, sym := range matched {
		b := blockForSymbol(rec.Lang, sym, lines)
		if b.endLine < b.startLine || b.startLine < 1 {
			continue
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return "", false
	}
	blocks = mergeOverlapping(blocks)

	return renderExcerpt(rec, lines, blocks), true
}

// matchingSymbols returns the subset of syms whose name (or, for qualified
// `Class.method` names, either component) lexically matches any of the
// query terms. Matching is the same shape the ranker uses for filenames
// in scoreFile: each side is expanded via ranking.TermVariants (raw +
// stem) and we accept either direction of substring containment.
//
// Why two directions:
//
//   - forward (term variant ⊆ symbol part): catches "rendering" → render*
//     because Stem("rendering") == "render".
//   - reverse (symbol part ⊆ term variant): catches "authentication" →
//     auth, "configuration" → config, where the user's query is a longer
//     compound and the symbol is the bare root.
//
// The 3-character floor on the reverse direction prevents pathological
// short symbols ("a", "go", "of") from matching every term — same floor
// the ranker applies to baseStem in scoreFile.
func matchingSymbols(syms []models.Symbol, terms []string) []models.Symbol {
	out := make([]models.Symbol, 0, len(syms))
	seen := make(map[string]bool, len(syms))

	// Pre-compute term variants once per call. Worst case 2 entries per term.
	tvars := make([][]string, 0, len(terms))
	for _, t := range terms {
		if t == "" {
			continue
		}
		tvars = append(tvars, ranking.TermVariants(t))
	}
	if len(tvars) == 0 {
		return out
	}

	for _, s := range syms {
		name := strings.ToLower(s.Name)
		parts := []string{name}
		if i := strings.Index(name, "."); i > 0 {
			parts = append(parts, name[:i], name[i+1:])
		}
		if !symbolPartsMatchAny(parts, tvars) {
			continue
		}
		key := s.Name + "|" + s.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// symbolPartsMatchAny reports whether any (part, term) combination satisfies
// the symmetric-containment predicate. Both sides are expanded via
// ranking.TermVariants, so a stem on one side meets a stem on the other.
// Containment is gated by a 3-character floor so two-letter accidents do
// not match.
func symbolPartsMatchAny(parts []string, tvars [][]string) bool {
	for _, part := range parts {
		if part == "" {
			continue
		}
		pvars := ranking.TermVariants(part)
		for _, p := range pvars {
			if len(p) < 3 {
				continue
			}
			for _, ts := range tvars {
				for _, t := range ts {
					if len(t) < 3 {
						continue
					}
					if strings.Contains(p, t) || strings.Contains(t, p) {
						return true
					}
				}
			}
		}
	}
	return false
}

// blockForSymbol dispatches to the right block walker for the file's
// language. Languages that do not delimit blocks textually (Markdown, JSON,
// YAML, unknown) get an empty block — caller drops them.
func blockForSymbol(lang models.Lang, sym models.Symbol, lines []string) block {
	switch lang {
	case models.LangTypeScript, models.LangJavaScript:
		return braceBlock(sym, lines)
	case models.LangPython:
		return indentBlock(sym, lines)
	}
	return block{}
}

// braceBlock walks from sym.Line forward, counts `{` / `}` while honouring
// `"` `'` ``` strings (with escape) and `//` `/* */` comments, and returns
// the first balanced body it finds. When the symbol is a one-liner (no
// brace at all on its line, e.g. an arrow assignment that ends with `;`)
// the block is just that single line.
//
// On any walk failure, returns a safe slice of up to 25 lines starting at
// sym.Line — wrong-but-bounded beats no excerpt at all.
func braceBlock(sym models.Symbol, lines []string) block {
	start := sym.Line
	if start < 1 || start > len(lines) {
		return block{}
	}
	depth := 0
	inString := false
	inLineComment := false
	inBlockComment := false
	var stringChar byte
	openedAny := false

	for li := start - 1; li < len(lines); li++ {
		line := lines[li]
		inLineComment = false // line comments reset at newline
		for i := 0; i < len(line); i++ {
			c := line[i]
			if inLineComment {
				continue
			}
			if inBlockComment {
				if c == '*' && i+1 < len(line) && line[i+1] == '/' {
					inBlockComment = false
					i++
				}
				continue
			}
			if inString {
				if c == '\\' && i+1 < len(line) {
					i++
					continue
				}
				if c == stringChar {
					inString = false
				}
				continue
			}
			switch c {
			case '"', '\'', '`':
				inString = true
				stringChar = c
			case '/':
				if i+1 < len(line) {
					switch line[i+1] {
					case '/':
						inLineComment = true
						i++
					case '*':
						inBlockComment = true
						i++
					}
				}
			case '{':
				depth++
				openedAny = true
			case '}':
				depth--
				if openedAny && depth == 0 {
					return block{startLine: start, endLine: li + 1, symbol: sym.Name}
				}
			case ';':
				// One-liner with no brace body (e.g. `const f = x => x + 1;`):
				// stop at the terminating semicolon, single-line excerpt.
				if !openedAny {
					return block{startLine: start, endLine: li + 1, symbol: sym.Name}
				}
			}
		}
	}
	end := start + 25
	if end > len(lines) {
		end = len(lines)
	}
	return block{startLine: start, endLine: end, symbol: sym.Name}
}

// indentBlock walks from sym.Line forward and returns the contiguous run of
// lines whose indentation is strictly greater than the header's, treating
// blank lines as belonging to the current block. Conservative: a comment
// line at the header's own indent terminates the block, matching how Python
// itself scopes a `def`/`class` body.
func indentBlock(sym models.Symbol, lines []string) block {
	start := sym.Line
	if start < 1 || start > len(lines) {
		return block{}
	}
	headIndent := leadingWidth(lines[start-1])
	end := start
	for li := start; li < len(lines); li++ {
		line := lines[li]
		if strings.TrimSpace(line) == "" {
			end = li + 1
			continue
		}
		if leadingWidth(line) <= headIndent {
			break
		}
		end = li + 1
	}
	return block{startLine: start, endLine: end, symbol: sym.Name}
}

// leadingWidth counts leading whitespace columns, treating tabs as 4. The
// exact width does not matter — only the strict-greater-than comparison in
// indentBlock does — so any consistent rule works.
func leadingWidth(line string) int {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// mergeOverlapping coalesces blocks that abut or overlap (gap ≤ 2 lines)
// into a single block, joining their symbol labels. This avoids printing
// two `// ──` markers separated by a one-line `// ... 1 lines omitted ...`,
// which costs tokens and reads worse than a continuous run.
//
// Used by the heuristic TS/JS/Python path. The Go path uses
// mergeOverlappingStrict instead so contiguous top-level decls (a const
// group then an unrelated func one line apart) are not silently fused
// into one giant block — go/ast already gives us exact ranges, no need
// to bridge the gap.
func mergeOverlapping(blocks []block) []block {
	return mergeBlocks(blocks, 2)
}

// mergeOverlappingStrict only coalesces blocks that genuinely overlap.
// Two adjacent-but-disjoint AST nodes (e.g. `type Options` ending at
// line 71 and `func Pack` starting at line 81) stay separate, so each
// one can be truncated by renderExcerpt independently and the rendered
// excerpt does not balloon to 130+ lines.
func mergeOverlappingStrict(blocks []block) []block {
	return mergeBlocks(blocks, 0)
}

// mergeBlocks is the shared engine. gapTolerance is the maximum number
// of empty lines allowed between two blocks before they get fused.
func mergeBlocks(blocks []block, gapTolerance int) []block {
	if len(blocks) <= 1 {
		return blocks
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].startLine < blocks[j].startLine })
	out := make([]block, 0, len(blocks))
	cur := blocks[0]
	for _, b := range blocks[1:] {
		if b.startLine <= cur.endLine+gapTolerance {
			if b.endLine > cur.endLine {
				cur.endLine = b.endLine
			}
			if !strings.Contains(cur.symbol, b.symbol) {
				cur.symbol = cur.symbol + ", " + b.symbol
			}
			continue
		}
		out = append(out, cur)
		cur = b
	}
	out = append(out, cur)
	return out
}

// renderOptions controls per-language tweaks to the excerpt rendering
// pipeline. The zero value matches the historical behaviour used by the
// heuristic TS/JS/Python path.
type renderOptions struct {
	// truncateBlocksOver, when > 0, replaces the body of any block
	// whose line span exceeds the threshold with
	// {excerptBlockHeadLines} + an elision marker + {excerptBlockTailLines}.
	// 0 disables truncation entirely (default).
	truncateBlocksOver int
}

// renderExcerpt assembles the final excerpt string with default options
// (no per-block truncation). Existing TS/JS/Python callers see no
// change in behaviour.
func renderExcerpt(rec models.FileRecord, lines []string, blocks []block) string {
	return renderExcerptWithOptions(rec, lines, blocks, renderOptions{})
}

// renderExcerptWithOptions is the configurable rendering entry point.
// The Go path calls it with truncateBlocksOver set, so an oversized
// function body is replaced with a head + elision + tail summary
// instead of forcing the whole excerpt past excerptMaxTokens and
// triggering a fallback to signature.
func renderExcerptWithOptions(rec models.FileRecord, lines []string, blocks []block, opts renderOptions) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "// file: %s\n", rec.RelPath)
	fmt.Fprintf(&sb, "// lang: %s\n", rec.Lang)
	fmt.Fprintf(&sb, "// representation: excerpt\n\n")

	totalLines := len(lines)
	prevEnd := 0
	for i, b := range blocks {
		if i == 0 && b.startLine > 1 {
			fmt.Fprintf(&sb, "// ... %d lines omitted from start ...\n\n", b.startLine-1)
		} else if i > 0 {
			gap := b.startLine - prevEnd - 1
			if gap > 0 {
				fmt.Fprintf(&sb, "\n// ... %d lines omitted ...\n\n", gap)
			}
		}
		fmt.Fprintf(&sb, "// ── %s:%d-%d (%s) ──\n", rec.RelPath, b.startLine, b.endLine, b.symbol)
		writeBlockBody(&sb, lines, b, opts.truncateBlocksOver)
		prevEnd = b.endLine
	}
	if prevEnd < totalLines {
		fmt.Fprintf(&sb, "\n// ... %d lines omitted to end ...\n", totalLines-prevEnd)
	}
	return sb.String()
}

// writeBlockBody emits the source lines for a block, optionally
// truncating the middle when the block is wider than truncateOver.
// truncateOver == 0 disables truncation (intact body).
func writeBlockBody(sb *strings.Builder, lines []string, b block, truncateOver int) {
	totalLines := len(lines)
	span := b.endLine - b.startLine + 1
	keepIntact := truncateOver <= 0 ||
		span <= truncateOver ||
		// Refuse to truncate when the head + tail would itself meet or
		// exceed the block — the elision marker would only add tokens.
		span <= excerptBlockHeadLines+excerptBlockTailLines+1
	if keepIntact {
		for li := b.startLine; li <= b.endLine && li-1 < totalLines; li++ {
			sb.WriteString(lines[li-1])
			sb.WriteByte('\n')
		}
		return
	}

	// Head: first excerptBlockHeadLines lines from the block.
	headEnd := b.startLine + excerptBlockHeadLines - 1
	for li := b.startLine; li <= headEnd && li-1 < totalLines; li++ {
		sb.WriteString(lines[li-1])
		sb.WriteByte('\n')
	}
	elided := span - excerptBlockHeadLines - excerptBlockTailLines
	fmt.Fprintf(sb, "// ... %d lines elided in extracted excerpt ...\n", elided)
	// Tail: last excerptBlockTailLines lines.
	tailStart := b.endLine - excerptBlockTailLines + 1
	for li := tailStart; li <= b.endLine && li-1 < totalLines; li++ {
		sb.WriteString(lines[li-1])
		sb.WriteByte('\n')
	}
}
