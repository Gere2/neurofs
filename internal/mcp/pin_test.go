package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResolveRepo_PinnedRejectsForeignRepo is the unit-level guard for
// CRIT-2: when the server is pinned to a root, a non-empty caller-supplied
// repo that does not canonicalise to that root must be refused.
func TestResolveRepo_PinnedRejectsForeignRepo(t *testing.T) {
	pinned := t.TempDir()
	ctx := withRepoRoot(context.Background(), pinned)

	if _, err := resolveRepo(ctx, "/etc"); err == nil {
		t.Fatal("resolveRepo must refuse /etc when pinned elsewhere")
	} else if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("error must mention pinning; got %v", err)
	}
}

// Empty repo arg under pinning falls through to the pinned root —
// that's how a legitimate caller signals "operate on the server's repo".
func TestResolveRepo_PinnedAcceptsEmpty(t *testing.T) {
	pinned := t.TempDir()
	ctx := withRepoRoot(context.Background(), pinned)

	got, err := resolveRepo(ctx, "")
	if err != nil {
		t.Fatalf("empty repo under pin must succeed; got %v", err)
	}
	if got != pinned {
		t.Errorf("got %q want %q", got, pinned)
	}
}

// A caller-supplied repo that canonicalises to the pinned root (e.g.
// trailing slash, ./, or a symlinked alias) is honoured.
func TestResolveRepo_PinnedAcceptsCanonicalMatch(t *testing.T) {
	pinned := t.TempDir()
	ctx := withRepoRoot(context.Background(), pinned)

	got, err := resolveRepo(ctx, pinned+string(filepath.Separator))
	if err != nil {
		t.Fatalf("trailing-slash form must match pinned root; got %v", err)
	}
	if got != pinned {
		t.Errorf("got %q want %q", got, pinned)
	}
}

// Without pinning (the legacy library/test path), the caller's repo arg
// controls. This preserves backwards compatibility for direct Server
// users that haven't called SetRepoRoot.
func TestResolveRepo_UnpinnedKeepsLegacyBehavior(t *testing.T) {
	tmp := t.TempDir()
	got, err := resolveRepo(context.Background(), tmp)
	if err != nil {
		t.Fatalf("unpinned with explicit repo must succeed; got %v", err)
	}
	want, _ := filepath.Abs(tmp)
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestPinned_ViewFileRejectsForeignRepoEndToEnd reproduces the agent's
// CRIT-2 attack against a real server with the same pin the CLI sets.
// A malicious caller tries to read /etc/passwd via neurofs_view_file
// and must get an error response, not file contents.
func TestPinned_ViewFileRejectsForeignRepoEndToEnd(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	srv := NewServer(inR, outW, io.Discard, "test")
	pinned := t.TempDir()
	srv.SetRepoRoot(pinned)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := srv.Run(ctx)
		outW.Close()
		done <- err
	}()

	go func() {
		defer inW.Close()
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neurofs_view_file","arguments":{"repo":"/etc","path":"passwd"}}}` + "\n"))
	}()

	var resp Response
	if err := json.NewDecoder(outR).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("call-level jsonrpc error (want tool-level isError): %+v", resp.Error)
	}
	var result ToolCallResult
	mustReencode(t, resp.Result, &result)
	if !result.IsError {
		t.Fatalf("attacker repo=/etc must be refused with IsError=true; got %+v", result)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "pinned") {
		t.Fatalf("error must explain pin; got %+v", result.Content)
	}
	// Critical: the response must NOT contain a passwd-like payload.
	body, _ := os.ReadFile("/etc/passwd")
	if len(body) > 100 && strings.Contains(result.Content[0].Text, "root:x:") {
		t.Fatalf("CRIT-2 not fixed: /etc/passwd contents leaked into response")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after ctx cancel")
	}
}

// TestPinned_SearchRejectsForeignRepoEndToEnd verifies that
// a malicious search call targeting /etc is rejected when the server is pinned.
func TestPinned_SearchRejectsForeignRepoEndToEnd(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	srv := NewServer(inR, outW, io.Discard, "test")
	pinned := t.TempDir()
	srv.SetRepoRoot(pinned)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := srv.Run(ctx)
		outW.Close()
		done <- err
	}()

	go func() {
		defer inW.Close()
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neurofs_search","arguments":{"repo":"/etc","query":"passwd"}}}` + "\n"))
	}()

	var resp Response
	if err := json.NewDecoder(outR).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("call-level jsonrpc error (want tool-level isError): %+v", resp.Error)
	}
	var result ToolCallResult
	mustReencode(t, resp.Result, &result)
	if !result.IsError {
		t.Fatalf("attacker repo=/etc must be refused with IsError=true; got %+v", result)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "pinned") {
		t.Fatalf("error must explain pin; got %+v", result.Content)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after ctx cancel")
	}
}
