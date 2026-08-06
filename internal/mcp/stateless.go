package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
)

// StatelessHandler creates an HTTP handler for MCP 2026 stateless requests over HTTP.
// It supports HTTP POST for JSON-RPC tool execution and GET /mcp/discover for capability discovery.
//
// repoRoot pins every path-taking tool, exactly as the stdio server does at
// startup. An empty repoRoot falls back to the process cwd rather than leaving
// the root unset: unset means "caller-controlled", which over HTTP would turn
// neurofs_view_file into an arbitrary host file reader via `{"repo": "/etc"}`
// (the CRIT-2 regression — see SetRepoRoot). There is no legitimate HTTP caller
// that needs an unpinned server, so the insecure case is simply unreachable.
//
// No CORS headers are emitted on purpose. A wildcard
// Access-Control-Allow-Origin here would let any page the user visits read the
// JSON-RPC response, which is a repo-content exfiltration channel; same-origin
// callers (the bundled UI) need no CORS header at all, and non-browser MCP
// clients do not enforce it. Origin/CSRF filtering is the caller's job — see
// safePost in internal/ui/api.go.
func StatelessHandler(repoRoot string, version string) http.HandlerFunc {
	if version == "" {
		version = protocolVersion
	}
	server := NewServer(nil, nil, os.Stderr, version)
	if repoRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			repoRoot = cwd
		}
	}
	// Fail closed: serving with an unpinned root is the arbitrary-read bug, so
	// a root we could not determine must refuse traffic rather than fall back.
	if repoRoot == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(errResponse(nil, codeInternalError, "mcp: repo root unavailable; refusing to serve unpinned", nil))
		}
	}
	server.SetRepoRoot(repoRoot)

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		// Support GET or /discover capability discovery (MCP July 2026 update)
		if r.Method == http.MethodGet || r.URL.Path == "/api/mcp/discover" {
			discoverRes := map[string]any{
				"mcp_version": version,
				"stateless":   true,
				"server_info": ServerInfo{Name: "neurofs-mcp", Version: version},
				"capabilities": map[string]any{
					"tools": true,
				},
				"tools": toolsList(),
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(discoverRes)
			return
		}

		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(errResponse(nil, codeInvalidRequest, "POST required for JSON-RPC tool calls", nil))
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil || len(bytes.TrimSpace(body)) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(errResponse(nil, codeInvalidRequest, "empty or unreadable body", nil))
			return
		}

		// The pin travels in the context, not on the Server struct: resolveRepo
		// reads it via repoRootFromCtx. Run() does this injection for the stdio
		// loop, so an HTTP path that called handle() with a bare context would
		// silently run unpinned even after SetRepoRoot.
		resp, drop := server.handle(withRepoRoot(r.Context(), repoRoot), body)
		if drop {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
