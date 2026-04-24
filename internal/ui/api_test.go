package ui

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)


func TestConfineToRepo_AcceptsRelativeInsideRepo(t *testing.T) {
	root := t.TempDir()
	got, err := confineToRepo(root, "audit/records/x.json", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// confineToRepo canonicalises through symlinks (e.g. /var → /private/var
	// on macOS); compare against the canonicalised join so the assertion
	// is symlink-agnostic.
	want := filepath.Join(resolveExistingPrefix(root), "audit/records/x.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfineToRepo_AcceptsAbsoluteInsideRepo(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "audit/records/x.json")
	got, err := confineToRepo(root, abs, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(resolveExistingPrefix(root), "audit/records/x.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfineToRepo_RejectsParentEscape(t *testing.T) {
	root := t.TempDir()
	_, err := confineToRepo(root, "../../etc/passwd", false)
	if err == nil {
		t.Fatalf("expected error for ../../etc/passwd")
	}
	if !strings.Contains(err.Error(), "inside the repo") {
		t.Fatalf("error should mention containment, got %q", err)
	}
}

func TestConfineToRepo_RejectsAbsoluteOutsideRepo(t *testing.T) {
	root := t.TempDir()
	_, err := confineToRepo(root, "/etc/passwd", false)
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
	_, err := confineToRepo(root, "audit/records/evil.json", false)
	if err == nil {
		t.Fatalf("expected symlink containment failure")
	}
}

func TestConfineToRepo_ForWriteAllowsMissingLeaf(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "audit/bundles"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	got, err := confineToRepo(root, "audit/bundles/new.json", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(resolveExistingPrefix(root), "audit/bundles/new.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestConfineToRepo_EmptyRejected(t *testing.T) {
	root := t.TempDir()
	_, err := confineToRepo(root, "  ", false)
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
