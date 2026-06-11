package indexer

import (
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
		kind  string
		start int
		end   int
	}{
		"export_class-user":       {kind: "export_class", start: 3, end: 9},
		"method-user.constructor": {kind: "method", start: 4, end: 4},
		"method-user.sayhello":    {kind: "method", start: 6, end: 8},
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
		if chunk.FilePath != "user.ts" {
			t.Errorf("chunk %s path = %q, want %q", chunk.ChunkID, chunk.FilePath, "user.ts")
		}
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
		kind  string
		start int
		end   int
	}{
		// The class chunk is capped at its header — methods carry their own
		// chunks, so the class body is never duplicated into one giant chunk.
		"class-calculator":      {kind: "class", start: 1, end: 1},
		"method-calculator.add": {kind: "method", start: 2, end: 4},
		"method-calculator.sub": {kind: "method", start: 6, end: 7},
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
