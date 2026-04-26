package mcp

import (
	"context"
	"encoding/json"
	"io"
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
	if len(listResult.Tools) != 2 {
		t.Fatalf("tools: got %d want 2", len(listResult.Tools))
	}
	names := []string{listResult.Tools[0].Name, listResult.Tools[1].Name}
	wantNames := map[string]bool{"neurofs_task": true, "neurofs_scan": true}
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
