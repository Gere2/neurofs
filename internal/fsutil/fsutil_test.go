package fsutil_test

import (
	"os"
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

// Regression: the security traffic agent's HIGH-1 — config-shaped
// files carrying secrets (no dot prefix) used to land verbatim in
// bundles. Confirm the deny-glob denies them now, and confirm source
// code with secret-flavoured names is still indexed.
func TestShouldSkipFile_SecretDenyGlob(t *testing.T) {
	skip := []string{
		// Config-shaped secret files (HIGH-1 was about these)
		"secrets.yaml",
		"secrets.yml",
		"secrets.json",
		"secrets.production.yaml",
		"credentials.json",
		"credentials-prod.yaml",
		"service-account.json",
		"service_account.json",
		// Exact basenames
		"id_rsa",
		"id_ed25519",
		"kubeconfig",
		// Cryptographic material by extension
		"server.pem",
		"private.key",
		"wildcard.crt",
		"client.p12",
		"vault.kdbx",
		// Dot-prefixed (already covered by the dot rule, but pin the
		// behaviour so a future cleanup doesn't accidentally regress).
		".env",
		".env.local",
		".npmrc",
	}
	keep := []string{
		// Source code with secret-flavoured names is NOT a config file
		// and must keep being indexed — denying these would hide real
		// production code in any project that has an auth/secrets pkg.
		"secrets.go",
		"credentials.ts",
		"secrets_test.go",
		"internal/secrets/manager.go",
		// .key as Go-related ext doesn't exist; "key.go" is just Go.
		"key.go",
		// .md is always kept (docs).
		"README.md",
		".github-policy.md",
	}
	for _, p := range skip {
		if !fsutil.ShouldSkipFile(p) {
			t.Errorf("ShouldSkipFile(%q) = false, want true (HIGH-1 regression: this would leak into bundles)", p)
		}
	}
	for _, p := range keep {
		if fsutil.ShouldSkipFile(p) {
			t.Errorf("ShouldSkipFile(%q) = true, want false (false positive — would hide legitimate source)", p)
		}
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

func TestIgnoreMatcher(t *testing.T) {
	tempDir := t.TempDir()

	ignoreContent := `
# ignore pos backup and next
*_backup/
.next
apps/noise.py
`
	err := os.WriteFile(filepath.Join(tempDir, ".neurofsignore"), []byte(ignoreContent), 0o644)
	if err != nil {
		t.Fatalf("failed to write ignore file: %v", err)
	}

	matcher := fsutil.LoadIgnoreMatcher(tempDir)

	// Test directory patterns
	if !matcher.Match("pos_v0_backup", true) {
		t.Error("expected pos_v0_backup directory to match")
	}
	if matcher.Match("pos_v0_backup", false) {
		t.Error("expected pos_v0_backup file not to match (directory-only pattern)")
	}

	// Test simple patterns
	if !matcher.Match(".next", true) {
		t.Error("expected .next directory to match")
	}
	if !matcher.Match(".next", false) {
		t.Error("expected .next file to match")
	}

	// Test slash patterns
	if !matcher.Match("apps/noise.py", false) {
		t.Error("expected apps/noise.py to match")
	}
	if matcher.Match("other/noise.py", false) {
		t.Error("expected other/noise.py NOT to match")
	}
}
