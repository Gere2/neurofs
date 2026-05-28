package fsutil_test

import (
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/models"
)

func TestLangForPath(t *testing.T) {
	cases := []struct {
		path string
		want models.Lang
	}{
		{"src/auth.ts", models.LangTypeScript},
		{"app/index.tsx", models.LangTypeScript},
		{"utils/helpers.js", models.LangJavaScript},
		{"server/index.mjs", models.LangJavaScript},
		{"scripts/deploy.py", models.LangPython},
		{"cmd/main.go", models.LangGo},
		{"README.md", models.LangMarkdown},
		{"docs/spec.mdx", models.LangMarkdown},
		{"config.json", models.LangJSON},
		{"config.yaml", models.LangYAML},
		{"config.yml", models.LangYAML},
		{"image.png", models.LangUnknown},
		{"bundle.min.js", models.LangUnknown},
		{"style.css", models.LangUnknown},
	}

	for _, tc := range cases {
		got := fsutil.LangForPath(tc.path)
		if got != tc.want {
			t.Errorf("LangForPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestShouldSkipDir(t *testing.T) {
	skip := []string{"node_modules", ".git", ".neurofs", ".claude", "audit", ".audit", "testdata", "fixtures", "vendor", "dist", "__pycache__"}
	keep := []string{"src", "internal", "docs", "tests"}

	for _, d := range skip {
		if !fsutil.ShouldSkipDir(d) {
			t.Errorf("ShouldSkipDir(%q) = false, want true", d)
		}
	}
	for _, d := range keep {
		if fsutil.ShouldSkipDir(d) {
			t.Errorf("ShouldSkipDir(%q) = true, want false", d)
		}
	}
}

// Regression: the ranking traffic agent surfaced that a blanket
// basename match on "audit" also hid internal/audit/ — the package
// implementing the entire audit/replay/BundleHash story. The path-aware
// variant must skip the top-level audit/ but keep nested ones indexable.
func TestShouldSkipDirAt_TopOnlyVsNested(t *testing.T) {
	root := "/repo"

	// Top-level "audit" must be skipped.
	if !fsutil.ShouldSkipDirAt(root, filepath.Join(root, "audit")) {
		t.Errorf("ShouldSkipDirAt root/audit must skip; got false")
	}

	// Nested internal/audit must NOT be skipped — that's source code.
	if fsutil.ShouldSkipDirAt(root, filepath.Join(root, "internal", "audit")) {
		t.Errorf("ShouldSkipDirAt internal/audit must NOT skip; got true (regression of the basename-match bug)")
	}

	// node_modules anywhere must still be skipped.
	if !fsutil.ShouldSkipDirAt(root, filepath.Join(root, "x", "node_modules")) {
		t.Errorf("node_modules at any depth must skip")
	}

	// testdata anywhere must still be skipped (Go convention).
	if !fsutil.ShouldSkipDirAt(root, filepath.Join(root, "internal", "pkg", "testdata")) {
		t.Errorf("testdata at any depth must skip")
	}
}

func TestCountLines(t *testing.T) {
	cases := []struct {
		input []byte
		want  int
	}{
		{[]byte(""), 0},
		{[]byte("one line"), 1},              // no trailing newline → 1
		{[]byte("line1\nline2"), 2},          // no trailing newline → 2
		{[]byte("line1\nline2\nline3\n"), 3}, // trailing newline → 3, not 4
		{[]byte("line1\n"), 1},               // single line with newline
	}

	for _, tc := range cases {
		got := fsutil.CountLines(tc.input)
		if got != tc.want {
			t.Errorf("CountLines(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}
