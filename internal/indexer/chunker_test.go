package indexer

import (
	"strings"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestBuildJSChunks(t *testing.T) {
	code := `import { Logger } from './logger';

export class User {
  constructor(public name: string) {}

  sayHello(): string {
    return "hello " + this.name;
  }
}

export function helper(a: number): number {
  return a * 2;
}

const x = 5;
`

	indexedAt := time.Now()
	chunks := BuildChunks("user.ts", "user.ts", models.LangTypeScript, code, indexedAt)

	expected := map[string]struct {
		kind     string
		start    int
		end      int
		parentID string
	}{
		// The class chunk is capped at the header so methods do not get
		// duplicated into one larger class chunk.
		"export_class-user":       {kind: "export_class", start: 3, end: 3},
		"method-user.constructor": {kind: "method", start: 4, end: 4, parentID: "export_class-user"},
		"method-user.sayhello":    {kind: "method", start: 6, end: 8, parentID: "export_class-user"},
		"export_func-helper":      {kind: "export_func", start: 11, end: 13},
	}

	if len(chunks) != len(expected) {
		t.Fatalf("expected %d chunks, got %d", len(expected), len(chunks))
	}

	for _, chunk := range chunks {
		exp, ok := expected[chunk.ChunkID]
		if !ok {
			t.Errorf("unexpected chunk ID: %s", chunk.ChunkID)
			continue
		}
		if chunk.Kind != exp.kind {
			t.Errorf("chunk %s kind = %q, want %q", chunk.ChunkID, chunk.Kind, exp.kind)
		}
		if chunk.StartLine != exp.start || chunk.EndLine != exp.end {
			t.Errorf("chunk %s line range = %d-%d, want %d-%d", chunk.ChunkID, chunk.StartLine, chunk.EndLine, exp.start, exp.end)
		}
		if chunk.ParentID != exp.parentID {
			t.Errorf("chunk %s parent = %q, want %q", chunk.ChunkID, chunk.ParentID, exp.parentID)
		}
		if chunk.FilePath != "user.ts" {
			t.Errorf("chunk %s path = %q, want %q", chunk.ChunkID, chunk.FilePath, "user.ts")
		}
	}
}

func TestBuildJSChunksExtractsNestedClosures(t *testing.T) {
	// The vuejs/core shape: a factory function whose inner const-assigned
	// closures are the real API. Padding pushes the body past the nested
	// scan threshold; the deeper closure must NOT be extracted (only the
	// shallowest layer is API surface).
	var b strings.Builder
	b.WriteString("function baseCreateRenderer(options: RendererOptions) {\n")
	for i := 0; i < 40; i++ {
		b.WriteString("  // padding line to cross the nested-scan threshold\n")
	}
	b.WriteString("  const mountComponent: MountComponentFn = (vnode, container) => {\n") // line 42
	b.WriteString("    const inner = (x: number) => {\n")
	b.WriteString("      return x\n")
	b.WriteString("    }\n")
	b.WriteString("    return inner(1)\n")
	b.WriteString("  }\n")                                              // line 47
	b.WriteString("  function patchChildren(n1: VNode, n2: VNode) {\n") // line 48
	b.WriteString("    return n1 === n2\n")
	b.WriteString("  }\n") // line 50
	b.WriteString("  return { mountComponent, patchChildren }\n")
	b.WriteString("}\n")

	chunks := BuildChunks("renderer.ts", "renderer.ts", models.LangTypeScript, b.String(), time.Now())

	bySymbol := make(map[string]models.Chunk, len(chunks))
	for _, c := range chunks {
		bySymbol[c.Symbol] = c
	}

	parent, ok := bySymbol["baseCreateRenderer"]
	if !ok {
		t.Fatalf("missing parent chunk; got %v", bySymbol)
	}
	mount, ok := bySymbol["baseCreateRenderer.mountComponent"]
	if !ok {
		t.Fatalf("nested const closure not extracted; got %v", bySymbol)
	}
	if mount.Kind != "nested_func" || mount.StartLine != 42 || mount.EndLine != 47 {
		t.Errorf("mountComponent = kind %q lines %d-%d, want nested_func 42-47", mount.Kind, mount.StartLine, mount.EndLine)
	}
	if mount.ParentID != parent.ChunkID {
		t.Errorf("mountComponent parent = %q, want %q", mount.ParentID, parent.ChunkID)
	}
	patch, ok := bySymbol["baseCreateRenderer.patchChildren"]
	if !ok {
		t.Fatalf("nested named function not extracted; got %v", bySymbol)
	}
	if patch.StartLine != 48 || patch.EndLine != 50 {
		t.Errorf("patchChildren lines %d-%d, want 48-50", patch.StartLine, patch.EndLine)
	}
	if _, exists := bySymbol["baseCreateRenderer.inner"]; exists {
		t.Error("second-level closure must not be extracted")
	}

	// Small function bodies are left alone.
	small := "function tiny() {\n  const helper = () => 1\n  return helper()\n}\n"
	for _, c := range BuildChunks("t.ts", "t.ts", models.LangTypeScript, small, time.Now()) {
		if c.Kind == "nested_func" {
			t.Errorf("nested chunk extracted from a small body: %s", c.Symbol)
		}
	}
}

func TestBuildGoChunksAssignsMethodParent(t *testing.T) {
	code := `package service

type Server struct {
	Name string
}

func (s *Server) Run() string {
	return s.Name
}
`

	chunks := BuildChunks("server.go", "server.go", models.LangGo, code, time.Now())

	byID := make(map[string]models.Chunk, len(chunks))
	for _, chunk := range chunks {
		byID[chunk.ChunkID] = chunk
	}

	if _, ok := byID["type-server"]; !ok {
		t.Fatalf("missing type chunk: %+v", chunks)
	}
	method, ok := byID["method-server.run"]
	if !ok {
		t.Fatalf("missing method chunk: %+v", chunks)
	}
	if method.ParentID != "type-server" {
		t.Fatalf("method parent = %q, want %q", method.ParentID, "type-server")
	}
}

func TestBuildJSChunksClassHeaderKeepsFields(t *testing.T) {
	code := `export class Context {
  readonly id = "ctx"

  get meta(): string { return this.id }
}
`

	chunks := BuildChunks("context.ts", "context.ts", models.LangTypeScript, code, time.Now())

	byID := make(map[string]models.Chunk, len(chunks))
	for _, chunk := range chunks {
		byID[chunk.ChunkID] = chunk
	}

	classChunk, ok := byID["export_class-context"]
	if !ok {
		t.Fatalf("missing class chunk: %+v", chunks)
	}
	if classChunk.StartLine != 1 || classChunk.EndLine != 2 {
		t.Fatalf("class chunk range = %d-%d, want 1-2: %+v", classChunk.StartLine, classChunk.EndLine, chunks)
	}

	getter, ok := byID["get-context.meta"]
	if !ok {
		t.Fatalf("missing getter chunk: %+v", chunks)
	}
	if getter.ParentID != "export_class-context" {
		t.Fatalf("getter parent = %q, want %q", getter.ParentID, "export_class-context")
	}
}

func TestBuildMarkdownChunksSplitsAtHeadings(t *testing.T) {
	doc := `Intro line before any heading.

# Title

Opening paragraph.

## First section

Body of the first section.

## Second section

Body of the second section.
More body.
`
	chunks := BuildChunks("doc.md", "doc.md", models.LangMarkdown, doc, time.Now())

	bySymbol := make(map[string]models.Chunk, len(chunks))
	for _, c := range chunks {
		if c.Kind != "section" {
			t.Errorf("chunk %q kind = %q, want section", c.Symbol, c.Kind)
		}
		bySymbol[c.Symbol] = c
	}
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d (%v), want 4 (preamble + title + 2 sections)", len(chunks), bySymbol)
	}
	if pre := bySymbol["preamble"]; pre.StartLine != 1 || pre.EndLine != 2 {
		t.Errorf("preamble = %d-%d, want 1-2", pre.StartLine, pre.EndLine)
	}
	if s := bySymbol["First section"]; s.StartLine != 7 || s.EndLine != 10 {
		t.Errorf("First section = %d-%d, want 7-10", s.StartLine, s.EndLine)
	}
	if s := bySymbol["Second section"]; s.StartLine != 11 || s.EndLine != 14 {
		t.Errorf("Second section = %d-%d, want 11-14", s.StartLine, s.EndLine)
	}

	// Headingless markdown keeps the whole-file fallback.
	plain := BuildChunks("notes.md", "notes.md", models.LangMarkdown, "just some text\nno headings\n", time.Now())
	if len(plain) != 1 || plain[0].Kind != "file" {
		t.Fatalf("headingless md = %+v, want single file chunk", plain)
	}
}

func TestBuildPythonChunks(t *testing.T) {
	code := `class Calculator:
    def add(self, a, b):
        # Adds two numbers
        return a + b

    def sub(self, a, b):
        return a - b

def helper():
    pass
`

	indexedAt := time.Now()
	chunks := BuildChunks("calc.py", "calc.py", models.LangPython, code, indexedAt)

	expectedPython := map[string]struct {
		kind     string
		start    int
		end      int
		parentID string
	}{
		// The class chunk is capped at its header — methods carry their own
		// chunks, so the class body is never duplicated into one giant chunk.
		"class-calculator":      {kind: "class", start: 1, end: 1},
		"method-calculator.add": {kind: "method", start: 2, end: 4, parentID: "class-calculator"},
		"method-calculator.sub": {kind: "method", start: 6, end: 7, parentID: "class-calculator"},
		"func-helper":           {kind: "func", start: 9, end: 10},
	}

	if len(chunks) != len(expectedPython) {
		t.Fatalf("expected %d chunks, got %d: %+v", len(expectedPython), len(chunks), chunks)
	}

	for _, chunk := range chunks {
		exp, ok := expectedPython[chunk.ChunkID]
		if !ok {
			t.Errorf("unexpected chunk ID: %s", chunk.ChunkID)
			continue
		}
		if chunk.Kind != exp.kind {
			t.Errorf("chunk %s kind = %q, want %q", chunk.ChunkID, chunk.Kind, exp.kind)
		}
		if chunk.StartLine != exp.start || chunk.EndLine != exp.end {
			t.Errorf("chunk %s line range = %d-%d, want %d-%d", chunk.ChunkID, chunk.StartLine, chunk.EndLine, exp.start, exp.end)
		}
		if chunk.ParentID != exp.parentID {
			t.Errorf("chunk %s parent = %q, want %q", chunk.ChunkID, chunk.ParentID, exp.parentID)
		}
	}
}

func TestBuildPythonChunksClassHeaderKeepsDocstring(t *testing.T) {
	code := `class Context:
    """Holds state for a single invocation."""

    allow_extra_args = False

    @property
    def meta(self):
        return self._meta
`

	chunks := BuildChunks("ctx.py", "ctx.py", models.LangPython, code, time.Now())

	byID := make(map[string]struct{ start, end int }, len(chunks))
	for _, c := range chunks {
		byID[c.ChunkID] = struct{ start, end int }{c.StartLine, c.EndLine}
	}

	// Header: class line, docstring, class attribute — but not the decorator,
	// which belongs to the method below it.
	if got, ok := byID["class-context"]; !ok || got.start != 1 || got.end != 4 {
		t.Fatalf("class header chunk = %+v (ok=%t), want 1-4: %+v", got, ok, chunks)
	}
	if got, ok := byID["method-context.meta"]; !ok || got.start != 7 || got.end != 8 {
		t.Fatalf("method chunk = %+v (ok=%t), want 7-8: %+v", got, ok, chunks)
	}
}
