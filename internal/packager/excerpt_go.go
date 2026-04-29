// Sub-file extraction for Go using go/parser + go/ast + go/token.
//
// Distinct from the heuristic brace/indent walkers in excerpt.go because
// Go has the strongest stdlib support for proper block extraction —
// go/ast gives exact line ranges, recognises every scope, and (crucially
// for our G3 fixtures) lets us discover struct field names that the
// regex parser in internal/parser does not expose as separate symbols.
//
// Why this matters: a query for "UpgradeWithSlack" or "PreferSignatures"
// — both of which are fields of packager.Options — found no matching
// symbol under the heuristic path because internal/parser only emits
// `type Options struct/interface { ... }` as a single top-level symbol.
// Here we walk the StructType / InterfaceType node and surface field
// names so the query brings the parent type's block.
//
// Scope is intentionally tight:
//   - top-level FuncDecl, GenDecl (TypeSpec, ValueSpec)
//   - struct fields and interface methods exposed for matching
//   - doc comments included in the extracted line range so the model
//     gets the full intent the author wrote, not just the keyword line
//   - per-spec granularity inside a GenDecl: a 30-element const block
//     where only one entry matches yields a one-line excerpt, not the
//     whole block
//
// Out of scope (by design): nested decls, generic constraints, build
// tags. They would all be welcome later but none of the v1 fixtures
// require them.
package packager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/ranking"
)

// extractGoExcerpt is the Go-specific extractor used when extractExcerpt
// dispatches a LangGo file. Returns ok=false when parsing fails, terms
// produce no variants, or no matching declaration is found — in any
// failure mode the caller falls back to signature, never to broken code.
func extractGoExcerpt(rec models.FileRecord, content string, terms []string) (string, bool) {
	tvars := buildTermVariants(terms)
	if len(tvars) == 0 {
		return "", false
	}

	fset := token.NewFileSet()
	// ParseComments so decl.Doc is populated — we want to include the
	// docstring above each block in the excerpt for free, since it is
	// already the most context-dense line in the file.
	// SkipObjectResolution: we never look up identifiers, so the
	// resolution pass is wasted work.
	file, err := parser.ParseFile(fset, rec.RelPath, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", false
	}

	var blocks []block
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if blk, ok := goFuncBlock(d, fset, tvars); ok {
				blocks = append(blocks, blk)
			}
		case *ast.GenDecl:
			blocks = append(blocks, goGenDeclBlocks(d, fset, tvars)...)
		}
	}

	if len(blocks) == 0 {
		return "", false
	}
	// Use the strict variant: go/ast already gives precise line ranges,
	// so two adjacent-but-disjoint top-level decls (e.g. `type Options`
	// ending at L71 and `func Pack` starting at L81) must NOT collapse
	// into a single 130-line block. mergeOverlapping's gap-tolerance
	// behaviour is reserved for the heuristic TS/JS/Python path.
	blocks = mergeOverlappingStrict(blocks)
	lines := strings.Split(content, "\n")
	return renderExcerptWithOptions(rec, lines, blocks, renderOptions{
		truncateBlocksOver: excerptBlockMaxLines,
	}), true
}

// goFuncBlock returns a block for a FuncDecl whose name matches any
// term, or ok=false otherwise. Receiver-bearing methods are matched by
// either the bare method name or the qualified Recv.method form, the
// same way the heuristic JS extractor handles class methods.
func goFuncBlock(d *ast.FuncDecl, fset *token.FileSet, tvars [][]string) (block, bool) {
	if d.Name == nil {
		return block{}, false
	}
	name := d.Name.Name
	candidates := []string{name}
	label := name
	if d.Recv != nil && len(d.Recv.List) > 0 {
		if r := receiverName(d.Recv.List[0].Type); r != "" {
			qualified := r + "." + name
			candidates = append(candidates, qualified, r)
			label = qualified
		}
	}
	if !anyNameMatches(candidates, tvars) {
		return block{}, false
	}

	startPos := d.Pos()
	if d.Doc != nil {
		startPos = d.Doc.Pos()
	}
	return block{
		startLine: fset.Position(startPos).Line,
		endLine:   fset.Position(d.End()).Line,
		symbol:    "func " + label,
	}, true
}

// goGenDeclBlocks expands a GenDecl into per-spec blocks. A const/type/var
// declaration with N specs that match contributes N blocks, each only as
// wide as the matching spec — this avoids dragging an unrelated
// 30-line const block into the bundle when only one entry matched.
//
// Doc comments attached to a parenthesised group's outer GenDecl
// (`// Group doc.\n const ( ... )`) are included on the first block of
// that group, since the outer doc usually documents the group's intent.
func goGenDeclBlocks(d *ast.GenDecl, fset *token.FileSet, tvars [][]string) []block {
	var out []block

	// One-spec form (`const X = 1`) reads .Pos() identically to the
	// per-spec path; we still go through the loop for uniform handling.
	for i, spec := range d.Specs {
		names, label := specCandidates(spec, d.Tok)
		if len(names) == 0 || !anyNameMatches(names, tvars) {
			continue
		}
		startPos := spec.Pos()
		// Spec doc takes precedence over GenDecl doc. The GenDecl doc
		// only attaches to the first spec when no per-spec doc exists,
		// to avoid printing the group preamble in front of every member.
		if doc := specDoc(spec); doc != nil {
			startPos = doc.Pos()
		} else if i == 0 && d.Doc != nil {
			startPos = d.Doc.Pos()
		}
		out = append(out, block{
			startLine: fset.Position(startPos).Line,
			endLine:   fset.Position(spec.End()).Line,
			symbol:    label,
		})
	}
	return out
}

// specCandidates returns the names that can match a query plus a
// human-readable block label. For type specs we additionally include
// struct field names and interface method names — that is the entire
// reason this Go path exists; without it, queries naming a field
// (UpgradeWithSlack) cannot find their parent type (Options).
func specCandidates(spec ast.Spec, tok token.Token) ([]string, string) {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		if s.Name == nil {
			return nil, ""
		}
		typeName := s.Name.Name
		names := []string{typeName}
		// Struct fields and interface methods feed matching but are NOT
		// part of the label — the label names the carrier type so the
		// reader's eye lands on the actual scope being shown.
		names = append(names, structFieldNames(s.Type)...)
		return names, "type " + typeName

	case *ast.ValueSpec:
		if len(s.Names) == 0 {
			return nil, ""
		}
		var names []string
		for _, n := range s.Names {
			names = append(names, n.Name)
		}
		kind := "const"
		if tok == token.VAR {
			kind = "var"
		}
		return names, kind + " " + strings.Join(names, ", ")
	}
	return nil, ""
}

// structFieldNames returns field names for *ast.StructType or method
// names for *ast.InterfaceType. Empty for any other expression. Embedded
// fields (no name) are skipped — the embedded type is its own top-level
// symbol elsewhere and a query for it will hit that decl directly.
func structFieldNames(e ast.Expr) []string {
	var out []string
	switch t := e.(type) {
	case *ast.StructType:
		if t.Fields == nil {
			return nil
		}
		for _, f := range t.Fields.List {
			for _, n := range f.Names {
				out = append(out, n.Name)
			}
		}
	case *ast.InterfaceType:
		if t.Methods == nil {
			return nil
		}
		for _, f := range t.Methods.List {
			for _, n := range f.Names {
				out = append(out, n.Name)
			}
		}
	}
	return out
}

// specDoc returns the doc comment attached to a single spec, when the
// author chose to document that spec individually (as opposed to the
// surrounding GenDecl group).
func specDoc(s ast.Spec) *ast.CommentGroup {
	switch x := s.(type) {
	case *ast.TypeSpec:
		return x.Doc
	case *ast.ValueSpec:
		return x.Doc
	}
	return nil
}

// receiverName returns the bare receiver type name from a FuncDecl
// receiver expression. Handles `*T` and `T`; everything else (generic
// type parameters in receivers, etc.) returns "" and the function falls
// back to the bare method name for matching.
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

// buildTermVariants pre-computes the variant set per term once per call.
// The same shape used by matchingSymbols in excerpt.go; kept in this
// file too so the Go path has no dependency on the heuristic path.
func buildTermVariants(terms []string) [][]string {
	out := make([][]string, 0, len(terms))
	for _, t := range terms {
		if t == "" {
			continue
		}
		out = append(out, ranking.TermVariants(t))
	}
	return out
}

// anyNameMatches reports whether any of the candidate names matches any
// term variant under the same symmetric-containment rule the heuristic
// path uses (3-character floor on both directions). Reuses
// symbolPartsMatchAny from excerpt.go so the matching policy is single-
// sourced.
func anyNameMatches(names []string, tvars [][]string) bool {
	for _, n := range names {
		if symbolPartsMatchAny([]string{strings.ToLower(n)}, tvars) {
			return true
		}
	}
	return false
}
