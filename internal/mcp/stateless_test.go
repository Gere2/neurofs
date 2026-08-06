package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatelessMCP_Discover(t *testing.T) {
	handler := StatelessHandler("", "2026-07-01")

	req := httptest.NewRequest("GET", "/api/mcp/discover", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var res map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode discover response: %v", err)
	}

	if res["stateless"] != true {
		t.Error("expected stateless: true")
	}
	if res["mcp_version"] != "2026-07-01" {
		t.Errorf("expected version 2026-07-01, got %v", res["mcp_version"])
	}
}

func TestStatelessMCP_JSONRPC(t *testing.T) {
	handler := StatelessHandler("", "2026-07-01")

	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	}
	bodyBytes, _ := json.Marshal(rpcReq)

	req := httptest.NewRequest("POST", "/api/mcp", bytes.NewReader(bodyBytes))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode RPC response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("expected no RPC error, got %v", resp.Error)
	}
}
