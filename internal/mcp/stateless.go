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
func StatelessHandler(repoRoot string, version string) http.HandlerFunc {
	if version == "" {
		version = protocolVersion
	}
	server := NewServer(nil, nil, os.Stderr, version)
	if repoRoot != "" {
		server.SetRepoRoot(repoRoot)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-MCP-Version")
			w.WriteHeader(http.StatusOK)
			return
		}

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

		resp, drop := server.handle(r.Context(), body)
		if drop {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
