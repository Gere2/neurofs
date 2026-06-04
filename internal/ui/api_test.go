package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/fsutil"
)

// canonicalRoot canonicalises an existing repo root through symlinks
// (matters on macOS: /var is a symlink to /private/var) so test assertions
// can compare against what ConfineToRepo would return.
func canonicalRoot(t *testing.T, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	return resolved
}

func TestConfineToRepo_AcceptsRelativeInsideRepo(t *testing.T) {
	root := t.TempDir()
	got, err := fsutil.ConfineToRepo(root, "audit/records/x.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(canonicalRoot(t, root), "audit/records/x.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfineToRepo_AcceptsAbsoluteInsideRepo(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "audit/records/x.json")
	got, err := fsutil.ConfineToRepo(root, abs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(canonicalRoot(t, root), "audit/records/x.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfineToRepo_RejectsParentEscape(t *testing.T) {
	root := t.TempDir()
	_, err := fsutil.ConfineToRepo(root, "../../etc/passwd")
	if err == nil {
		t.Fatalf("expected error for ../../etc/passwd")
	}
	if !strings.Contains(err.Error(), "inside the repo") {
		t.Fatalf("error should mention containment, got %q", err)
	}
}

func TestConfineToRepo_RejectsAbsoluteOutsideRepo(t *testing.T) {
	root := t.TempDir()
	_, err := fsutil.ConfineToRepo(root, "/etc/passwd")
	if err == nil {
		t.Fatalf("expected error for absolute escape")
	}
}

func TestConfineToRepo_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("hunter2"), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	linkDir := filepath.Join(root, "audit", "records")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	linkPath := filepath.Join(linkDir, "evil.json")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := fsutil.ConfineToRepo(root, "audit/records/evil.json")
	if err == nil {
		t.Fatalf("expected symlink containment failure")
	}
}

func TestConfineToRepo_AllowsMissingLeaf(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "audit/bundles"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	got, err := fsutil.ConfineToRepo(root, "audit/bundles/new.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(canonicalRoot(t, root), "audit/bundles/new.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfineToRepo_EmptyRejected(t *testing.T) {
	root := t.TempDir()
	_, err := fsutil.ConfineToRepo(root, "  ")
	if err == nil {
		t.Fatalf("expected error for empty path")
	}
}

func TestClampAnnotation_TruncatesAtRuneBoundary(t *testing.T) {
	// "你" is 3 bytes in UTF-8. Clamping at max=10 on "你你你你" (12 bytes)
	// without rune-aware truncation would slice a byte into the middle of
	// the 4th character and produce invalid UTF-8.
	in := "你你你你"
	out := clampAnnotation(in, 10)
	if !isValidUTF8(out) {
		t.Fatalf("result must be valid UTF-8, got %q", out)
	}
	// At 10 bytes cap, three 3-byte runes fit (9 bytes). The 4th would
	// bring us to 12, so the last rune must be dropped rather than split.
	if len(out) != 9 {
		t.Fatalf("expected 9 bytes (3 runes kept), got %d (%q)", len(out), out)
	}
}

func TestClampAnnotation_UnderCapUnchanged(t *testing.T) {
	in := "short ascii"
	out := clampAnnotation(in, 200)
	if out != in {
		t.Fatalf("got %q want %q", out, in)
	}
}

func TestClampAnnotation_TrimsWhitespace(t *testing.T) {
	out := clampAnnotation("   padded   ", 200)
	if out != "padded" {
		t.Fatalf("expected whitespace trimmed, got %q", out)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && !strings.Contains(s, string(rune(0xFFFD))) {
			return false
		}
	}
	// range-over-string replaces invalid sequences with U+FFFD; re-encoding
	// and comparing byte length is a stricter check.
	return len([]byte(s)) == len([]byte(string([]rune(s))))
}

// TestDecode_RejectsTrailingContent locks in the "exactly one JSON object
// per body" contract. A second object smuggled after the first must be
// rejected — otherwise a handler that only reads the first could process
// attacker-controlled input while believing the request was well-formed.
func TestDecode_RejectsTrailingContent(t *testing.T) {
	body := `{"repo":"/tmp/x"}{"repo":"/etc/passwd"}`
	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(body))

	var got struct {
		Repo string `json:"repo"`
	}
	err := decode(r, &got)
	if err == nil {
		t.Fatalf("expected trailing content to be rejected, got nil error (parsed repo=%q)", got.Repo)
	}
	if !strings.Contains(err.Error(), "single JSON object") {
		t.Fatalf("expected 'single JSON object' error, got: %v", err)
	}
}

// TestDecode_AcceptsSingleObject is the positive counterpart: one
// well-formed object must still decode cleanly with no spurious EOF noise.
func TestDecode_AcceptsSingleObject(t *testing.T) {
	body := `{"repo":"/tmp/x","verbose":true}`
	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(body))

	var got scanReq
	if err := decode(r, &got); err != nil {
		t.Fatalf("expected clean decode, got: %v", err)
	}
	if got.Repo != "/tmp/x" || !got.Verbose {
		t.Fatalf("unexpected decoded value: %+v", got)
	}
}

// TestDecode_TrailingWhitespaceIsFine guards against over-strictness:
// a body that ends with a newline or spaces is common from curl/fetch
// and must not be treated as trailing content.
func TestDecode_TrailingWhitespaceIsFine(t *testing.T) {
	body := "{\"repo\":\"/tmp/x\"}\n  \t\n"
	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(body))

	var got scanReq
	if err := decode(r, &got); err != nil && err != io.EOF {
		t.Fatalf("trailing whitespace should decode cleanly, got: %v", err)
	}
}

func TestMustRepo_SandboxEnforcement(t *testing.T) {
	tempDir1 := t.TempDir()
	tempDir2 := t.TempDir()

	// Create .neurofs directories to pass validation
	if err := os.MkdirAll(filepath.Join(tempDir1, ".neurofs"), 0755); err != nil {
		t.Fatalf("failed to create db dir 1: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tempDir2, ".neurofs"), 0755); err != nil {
		t.Fatalf("failed to create db dir 2: %v", err)
	}

	// Ensure they are canonicalized
	repo1 := canonicalRoot(t, tempDir1)
	repo2 := canonicalRoot(t, tempDir2)

	// Enable sandboxing
	sandboxActive = true
	pinnedRepo = repo1
	defer func() {
		sandboxActive = false
		pinnedRepo = ""
	}()

	// Case 1: repo parameter matches pinnedRepo -> should succeed
	rr := httptest.NewRecorder()
	cfg, ok := mustRepo(rr, repo1)
	if !ok {
		t.Fatalf("expected mustRepo to succeed for pinned directory, got error: %s", rr.Body.String())
	}
	if cfg.RepoRoot != repo1 {
		t.Errorf("expected RepoRoot to be %q, got %q", repo1, cfg.RepoRoot)
	}

	// Case 2: repo parameter is different -> should fail with 403
	rr2 := httptest.NewRecorder()
	_, ok2 := mustRepo(rr2, repo2)
	if ok2 {
		t.Fatalf("expected mustRepo to fail for non-pinned directory")
	}
	if rr2.Code != http.StatusForbidden {
		t.Errorf("expected status 403 Forbidden, got %d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "access denied") {
		t.Errorf("expected error message to contain 'access denied', got: %s", rr2.Body.String())
	}
}
