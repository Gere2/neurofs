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

func TestServerHandshakeAndDispatch(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	srv := NewServer(inR, outW, io.Discard, "test")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := srv.Run(ctx)
		outW.Close()
		done <- err
	}()

	go func() {
		defer inW.Close()
		msgs := []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"neurofs_unknown","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"does/not/exist"}`,
		}
		for _, m := range msgs {
			if _, err := inW.Write([]byte(m + "\n")); err != nil {
				return
			}
		}
	}()

	dec := json.NewDecoder(outR)

	// 1) initialize
	var initResp Response
	if err := dec.Decode(&initResp); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize error: %+v", initResp.Error)
	}
	if string(initResp.ID) != "1" {
		t.Fatalf("initialize id: got %s want 1", initResp.ID)
	}
	var initResult InitializeResult
	mustReencode(t, initResp.Result, &initResult)
	if initResult.ProtocolVersion != protocolVersion {
		t.Fatalf("protocolVersion: got %q want %q", initResult.ProtocolVersion, protocolVersion)
	}
	if initResult.ServerInfo.Name != "neurofs" {
		t.Fatalf("serverInfo.name: got %q", initResult.ServerInfo.Name)
	}
	if initResult.ServerInfo.Version != "test" {
		t.Fatalf("serverInfo.version: got %q want test", initResult.ServerInfo.Version)
	}

	// 2) tools/list (notifications/initialized produced no output)
	var listResp Response
	if err := dec.Decode(&listResp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if listResp.Error != nil {
		t.Fatalf("tools/list error: %+v", listResp.Error)
	}
	var listResult ToolsListResult
	mustReencode(t, listResp.Result, &listResult)
	if len(listResult.Tools) != 3 {
		t.Fatalf("tools: got %d want 3", len(listResult.Tools))
	}
	names := []string{listResult.Tools[0].Name, listResult.Tools[1].Name, listResult.Tools[2].Name}
	wantNames := map[string]bool{"neurofs_task": true, "neurofs_scan": true, "neurofs_view_file": true}
	for _, n := range names {
		if !wantNames[n] {
			t.Fatalf("unexpected tool name %q in %v", n, names)
		}
	}

	// 3) tools/call with unknown tool name → isError true, success response shape
	var callResp Response
	if err := dec.Decode(&callResp); err != nil {
		t.Fatalf("decode tools/call: %v", err)
	}
	if callResp.Error != nil {
		t.Fatalf("call response carried jsonrpc error, want tool-level isError: %+v", callResp.Error)
	}
	var callResult ToolCallResult
	mustReencode(t, callResp.Result, &callResult)
	if !callResult.IsError {
		t.Fatalf("expected isError=true for unknown tool, got %+v", callResult)
	}
	if len(callResult.Content) == 0 || !strings.Contains(callResult.Content[0].Text, "unknown tool") {
		t.Fatalf("expected unknown-tool message, got %+v", callResult.Content)
	}

	// 4) unknown method → -32601
	var unkResp Response
	if err := dec.Decode(&unkResp); err != nil {
		t.Fatalf("decode unknown method: %v", err)
	}
	if unkResp.Error == nil || unkResp.Error.Code != codeMethodNotFound {
		t.Fatalf("expected method-not-found error, got %+v", unkResp)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after stdin closed")
	}
}

func mustReencode(t *testing.T, src any, dst any) {
	t.Helper()
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
}

func TestViewFileTool(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	filePath := "hello.txt"
	absPath := filepath.Join(tmpDir, filePath)
	content := "Hello from NeuroFS!"
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	// 1) Test reading file successfully
	args := map[string]any{
		"path": filePath,
		"repo": tmpDir,
	}
	rawArgs, _ := json.Marshal(args)
	res := runViewFileTool(ctx, rawArgs)
	if res.IsError {
		t.Fatalf("expected view file to succeed, got error: %s", res.Content[0].Text)
	}
	if res.Content[0].Text != content {
		t.Errorf("expected content %q, got %q", content, res.Content[0].Text)
	}

	// 2) Test reading non-existent file
	argsNonExistent := map[string]any{
		"path": "missing.txt",
		"repo": tmpDir,
	}
	rawArgsNonExistent, _ := json.Marshal(argsNonExistent)
	resNonExistent := runViewFileTool(ctx, rawArgsNonExistent)
	if !resNonExistent.IsError {
		t.Fatalf("expected error reading missing file")
	}

	// 3) Test path traversal containment
	argsEscape := map[string]any{
		"path": "../secret.txt",
		"repo": tmpDir,
	}
	rawArgsEscape, _ := json.Marshal(argsEscape)
	resEscape := runViewFileTool(ctx, rawArgsEscape)
	if !resEscape.IsError {
		t.Fatalf("expected path traversal to be blocked")
	}
	if !strings.Contains(resEscape.Content[0].Text, "path must live inside the repo") &&
		!strings.Contains(resEscape.Content[0].Text, "does not exist") {
		t.Errorf("expected path containment or existence error, got: %q", resEscape.Content[0].Text)
	}
}
