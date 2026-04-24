package ui

import (
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
