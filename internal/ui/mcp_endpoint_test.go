package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpMux builds the real route table pinned to a scratch repo, so these
// tests exercise the same wiring production uses rather than a stand-in.
func mcpMux(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "in_repo.txt"), []byte("in-repo content\n"), 0o644); err != nil {
		t.Fatalf("seed repo file: %v", err)
	}
	mux := http.NewServeMux()
	registerAPI(mux, originsForAddr("127.0.0.1:7777"), repo)
	return mux, repo
}

func toolCall(name, repo, path string) string {
	args := map[string]string{"path": path}
	if repo != "" {
		args["repo"] = repo
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	return string(body)
}

// A page at evil.com must not be able to drive MCP tool calls against the
// user's loopback server. Before the fix /api/mcp was registered bare, so
// this exact request returned 200 with the file's contents.
func TestMCPEndpoint_CrossOriginPostRejected(t *testing.T) {
	mux, _ := mcpMux(t)

	r := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(toolCall("neurofs_view_file", "", "in_repo.txt")))
	r.Header.Set("Origin", "https://evil.com")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin MCP tools/call must be 403; got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "in-repo content") {
		t.Fatalf("cross-origin caller must never receive repo contents: %s", w.Body.String())
	}
}

// Sec-Fetch-Site catches the attacker page that strips Origin by using a
// CORS-preflight-skipping content type.
func TestMCPEndpoint_CrossSiteFetchRejected(t *testing.T) {
	mux, _ := mcpMux(t)

	r := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(toolCall("neurofs_view_file", "", "in_repo.txt")))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch to /api/mcp must be 403; got %d", w.Code)
	}
}

// The wildcard Access-Control-Allow-Origin was the second half of the bug:
// even a rejected request must not tell the browser to expose the response,
// and an accepted same-origin one does not need the header at all.
func TestMCPEndpoint_NoWildcardCORS(t *testing.T) {
	mux, _ := mcpMux(t)

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/mcp"},
		{"GET", "/api/mcp/discover"},
		{"OPTIONS", "/api/mcp"},
		{"GET", "/.well-known/agent.json"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s %s must not send Access-Control-Allow-Origin; got %q", tc.method, tc.path, got)
		}
	}
}

// CRIT-2 regression: an explicit `repo` argument must not escape the pinned
// root. This is the request that read /etc/hosts off the host before the fix.
func TestMCPEndpoint_ArbitraryRepoArgumentRefused(t *testing.T) {
	mux, _ := mcpMux(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-CANARY"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	// Same-origin, so only the path pinning can stop it.
	r := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(toolCall("neurofs_view_file", outside, "secret.txt")))
	r.Header.Set("Origin", "http://127.0.0.1:7777")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if strings.Contains(w.Body.String(), "TOP-SECRET-CANARY") {
		t.Fatalf("an out-of-root repo argument must not be honoured; got %s", w.Body.String())
	}
}

// The legitimate path must keep working: the bundled UI calls this
// same-origin, and a pinned in-repo read should still succeed.
func TestMCPEndpoint_SameOriginInRepoReadAllowed(t *testing.T) {
	mux, _ := mcpMux(t)

	r := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(toolCall("neurofs_view_file", "", "in_repo.txt")))
	r.Header.Set("Origin", "http://127.0.0.1:7777")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("same-origin in-repo read must succeed; got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "in-repo content") {
		t.Fatalf("expected file contents in response; got %s", w.Body.String())
	}
}

// /api/orchestrate/models POST writes models.json, so it needs the same
// Origin check as every other state-changing endpoint.
func TestOrchestrateModels_CrossOriginPostRejected(t *testing.T) {
	mux, _ := mcpMux(t)

	r := httptest.NewRequest("POST", "/api/orchestrate/models", strings.NewReader(`{"routing":{"backend":"pwned"}}`))
	r.Header.Set("Origin", "https://evil.com")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin models POST must be 403; got %d", w.Code)
	}
}
