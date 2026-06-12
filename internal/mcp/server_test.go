package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/contextladder"
	"github.com/neuromfs/neuromfs/internal/contextmap"
	"github.com/neuromfs/neuromfs/internal/contextusage"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/memory"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/storage"
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
	if len(listResult.Tools) != 15 {
		t.Fatalf("tools: got %d want 15", len(listResult.Tools))
	}
	wantNames := map[string]bool{
		"neurofs_context":         true,
		"neurofs_task":            true,
		"neurofs_scan":            true,
		"neurofs_view_file":       true,
		"neurofs_get_outline":     true,
		"neurofs_expand":          true,
		"neurofs_measure":         true,
		"neurofs_list_signatures": true,
		"neurofs_get_excerpt":     true,
		"neurofs_search":          true,
		"neurofs_log_memory":      true,
		"neurofs_search_memory":   true,
		"neurofs_export_memory":   true,
		"neurofs_prune_memory":    true,
		"neurofs_recall_state":    true,
	}
	for _, tool := range listResult.Tools {
		if !wantNames[tool.Name] {
			t.Fatalf("unexpected tool name %q", tool.Name)
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

// Regression: the MCP traffic agent surfaced that the server returned
// full responses to notifications (JSON-RPC §4.1 violation). Send a
// `tools/list` notification (no id) immediately followed by a normal
// `tools/list` request (id=2). The wire must show only one response,
// and that response must be for id=2.
func TestNotificationsGetNoResponse(t *testing.T) {
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
		// Notification (no id) — must be silently swallowed.
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","method":"tools/list"}` + "\n"))
		// Notification of initialize — same.
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","method":"initialize","params":{}}` + "\n"))
		// Real request — id=2. Should be the ONLY response on the wire.
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"))
	}()

	dec := json.NewDecoder(outR)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if string(resp.ID) != "2" {
		t.Fatalf("first response id = %s, want 2 — server is answering notifications (JSON-RPC §4.1 violation)", resp.ID)
	}

	// Confirm no further responses came. The server should be alive but
	// idle. Close stdin and confirm we hit EOF, not another response.
	if err := dec.Decode(&resp); err != io.EOF {
		t.Fatalf("expected EOF after only request answered, got resp id=%s err=%v", resp.ID, err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after stdin closed")
	}
}

// Regression: the MCP traffic agent surfaced that a single stdin line
// larger than the bufio.Scanner buffer cap (was 4 MiB) killed the
// server permanently. Multi-megabyte messages (a long search response,
// or a host-side prompt context) must not kill the server.
func TestServerSurvivesLargeMessage(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	srv := NewServer(inR, outW, io.Discard, "test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := srv.Run(ctx)
		outW.Close()
		done <- err
	}()

	// 5 MiB filler — well past the prior 4 MiB cap. Embed it as an
	// argument string in a syntactically valid tools/call; the server
	// will return an unknown-tool error, but it must NOT crash.
	filler := strings.Repeat("a", 5*1024*1024)
	bigMsg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neurofs_unknown","arguments":{"query":"` + filler + `"}}}`

	go func() {
		defer inW.Close()
		if _, err := inW.Write([]byte(bigMsg + "\n")); err != nil {
			return
		}
		// Follow-up normal message to prove the server is still alive.
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"))
	}()

	dec := json.NewDecoder(outR)

	// 1) response to the big message — any well-formed response is OK;
	// the only failure mode we care about is "no response at all" because
	// scanner died.
	var bigResp Response
	if err := dec.Decode(&bigResp); err != nil {
		t.Fatalf("server did not respond to 5 MiB message (scanner buffer cap?): %v", err)
	}
	if string(bigResp.ID) != "1" {
		t.Fatalf("big-message response id: got %s want 1", bigResp.ID)
	}

	// 2) follow-up tools/list — server is still alive.
	var listResp Response
	if err := dec.Decode(&listResp); err != nil {
		t.Fatalf("server died after large message — follow-up tools/list got: %v", err)
	}
	if listResp.Error != nil {
		t.Fatalf("tools/list error after large message: %+v", listResp.Error)
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

func TestContextToolRoutesSearchAndBundle(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	goCode := `package service

func AlphaWorker() string {
	return "alpha"
}

func BetaWorker() string {
	return "beta"
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "service.go"), []byte(goCode), 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query":  "AlphaWorker",
		"repo":   tmpDir,
		"intent": "search",
		"limit":  3,
	})
	searchRes := runContextTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("context search failed: %s", searchRes.Content[0].Text)
	}
	var searchPayload ContextResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &searchPayload); err != nil {
		t.Fatalf("decode context search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if searchPayload.Route != "search" || !traceHasTool(searchPayload.ToolTrace, "neurofs_search") {
		t.Fatalf("unexpected context search route: %+v", searchPayload)
	}
	if len(searchPayload.StructuralHints) == 0 || searchPayload.StructuralHints[0].Path != "service.go" {
		t.Fatalf("expected structural hint for service.go, got %+v", searchPayload.StructuralHints)
	}
	if len(searchPayload.StructuralHints[0].SymbolMatches) == 0 || searchPayload.StructuralHints[0].SymbolMatches[0].Name != "AlphaWorker" {
		t.Fatalf("expected AlphaWorker structural symbol, got %+v", searchPayload.StructuralHints[0])
	}
	if len(searchPayload.Results) == 0 || searchPayload.Results[0].Symbol != "AlphaWorker" {
		t.Fatalf("expected AlphaWorker search result, got %+v", searchPayload.Results)
	}
	if !containsString(searchPayload.Results[0].Reasons, "structural_symbol") {
		t.Fatalf("expected structural_symbol boost reason, got %+v", searchPayload.Results[0].Reasons)
	}

	autoArgsRaw, _ := json.Marshal(map[string]any{
		"query": "AlphaWorker location",
		"repo":  tmpDir,
	})
	autoRes := runContextTool(ctx, autoArgsRaw)
	if autoRes.IsError {
		t.Fatalf("context auto route failed: %s", autoRes.Content[0].Text)
	}
	var autoPayload ContextResponse
	if err := json.Unmarshal([]byte(autoRes.Content[0].Text), &autoPayload); err != nil {
		t.Fatalf("decode context auto payload: %v\n%s", err, autoRes.Content[0].Text)
	}
	if autoPayload.Route != "excerpt" || autoPayload.Intent != "excerpt" {
		t.Fatalf("expected structural symbol query to route to excerpt, got %+v", autoPayload)
	}
	if !strings.Contains(autoPayload.Text, "AlphaWorker") {
		t.Fatalf("expected excerpt text to contain AlphaWorker, got:\n%s", autoPayload.Text)
	}
	if got := inferContextIntent("BuildChunks location"); got != "search" {
		t.Fatalf("symbol-like query should not be treated as build intent, got %q", got)
	}

	researchArgsRaw, _ := json.Marshal(map[string]any{
		"query":  "Worker",
		"repo":   tmpDir,
		"intent": "research",
		"limit":  1,
	})
	researchRes := runContextTool(ctx, researchArgsRaw)
	if researchRes.IsError {
		t.Fatalf("context research route failed: %s", researchRes.Content[0].Text)
	}
	var researchPayload ContextResponse
	if err := json.Unmarshal([]byte(researchRes.Content[0].Text), &researchPayload); err != nil {
		t.Fatalf("decode context research payload: %v\n%s", err, researchRes.Content[0].Text)
	}
	if researchPayload.Intent != "research" || researchPayload.Route != "search" {
		t.Fatalf("expected research profile to use search route, got %+v", researchPayload)
	}
	if len(researchPayload.Results) < 2 {
		t.Fatalf("expected research profile to widen limit beyond 1, got %+v", researchPayload.Results)
	}
	if !traceReasonContains(researchPayload.ToolTrace, "research profile") {
		t.Fatalf("expected research trace reason, got %+v", researchPayload.ToolTrace)
	}

	bundleArgsRaw, _ := json.Marshal(map[string]any{
		"query":  "Where is AlphaWorker implemented?",
		"repo":   tmpDir,
		"intent": "build",
		"budget": 1200,
	})
	bundleRes := runContextTool(ctx, bundleArgsRaw)
	if bundleRes.IsError {
		t.Fatalf("context bundle failed: %s", bundleRes.Content[0].Text)
	}
	var bundlePayload ContextResponse
	if err := json.Unmarshal([]byte(bundleRes.Content[0].Text), &bundlePayload); err != nil {
		t.Fatalf("decode context bundle payload: %v\n%s", err, bundleRes.Content[0].Text)
	}
	if bundlePayload.Route != "task_chunks" || bundlePayload.PromptPath == "" || bundlePayload.BundlePath == "" {
		t.Fatalf("unexpected context bundle route: %+v", bundlePayload)
	}
	if !strings.Contains(bundlePayload.Prompt, `rep="excerpt"`) || !strings.Contains(bundlePayload.Prompt, "AlphaWorker") {
		t.Fatalf("expected chunk excerpt prompt, got:\n%s", bundlePayload.Prompt)
	}
}

func TestSearchToolReturnsPersistedGoChunks(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	goCode := `package service

func AlphaWorker() string {
	return "alpha"
}

func BetaWorker() string {
	return "beta"
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "service.go"), []byte(goCode), 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "alpha",
		"repo":  tmpDir,
		"limit": 3,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("expected at least one search result")
	}
	top := payload.Results[0]
	if top.Symbol != "AlphaWorker" {
		t.Fatalf("expected AlphaWorker first, got %+v", top)
	}
	if !strings.Contains(top.Snippet, "AlphaWorker") || strings.Contains(top.Snippet, "BetaWorker") {
		t.Fatalf("expected alpha-only chunk snippet, got:\n%s", top.Snippet)
	}
	if !containsString(top.Reasons, "symbol_match") {
		t.Fatalf("expected symbol_match reason, got %+v", top.Reasons)
	}

	excArgsRaw, _ := json.Marshal(map[string]any{
		"path":  "service.go",
		"query": "alpha",
		"repo":  tmpDir,
	})
	excRes := runGetExcerptTool(ctx, excArgsRaw)
	if excRes.IsError {
		t.Fatalf("get excerpt failed: %s", excRes.Content[0].Text)
	}
	if !strings.Contains(excRes.Content[0].Text, "source: persisted_chunks") {
		t.Fatalf("expected persisted chunk excerpt, got:\n%s", excRes.Content[0].Text)
	}
	if !strings.Contains(excRes.Content[0].Text, "AlphaWorker") || strings.Contains(excRes.Content[0].Text, "BetaWorker") {
		t.Fatalf("expected excerpt to include AlphaWorker only, got:\n%s", excRes.Content[0].Text)
	}
}

func TestSearchToolUsesSemanticChunkEmbeddings(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	goCode := `package service

func AlphaWorker() string {
	return "alpha"
}

func BetaWorker() string {
	return "beta"
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "service.go"), []byte(goCode), 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	cfg, err := config.New(tmpDir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	rec, err := db.GetFileByRelPath("service.go")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	chunks, err := db.GetChunksForFile(rec.Path)
	if err != nil {
		t.Fatalf("get chunks: %v", err)
	}

	var betaHash string
	for _, chunk := range chunks {
		if chunk.Symbol == "BetaWorker" {
			betaHash = chunk.ContentHash
			break
		}
	}
	if betaHash == "" {
		t.Fatalf("expected BetaWorker chunk in %+v", chunks)
	}

	embClient := embeddings.NewClient()
	queryEmbedding, err := embClient.GetEmbedding(ctx, "semantic needle")
	if err != nil {
		t.Fatalf("query embedding: %v", err)
	}
	if err := db.SaveChunkEmbedding(betaHash, queryEmbedding, embClient.ProviderName(), embClient.ModelName()); err != nil {
		t.Fatalf("save forced chunk embedding: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "semantic needle",
		"repo":  tmpDir,
		"limit": 2,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("expected semantic search result")
	}
	top := payload.Results[0]
	if top.Symbol != "BetaWorker" {
		t.Fatalf("expected BetaWorker first from semantic match, got %+v", top)
	}
	if !containsString(top.Reasons, "semantic_match") {
		t.Fatalf("expected semantic_match reason, got %+v", top.Reasons)
	}
}

func TestSearchToolAddsDependencyGraphBridge(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	appCode := `import { helper } from "./util";

export function startApp() {
  return helper();
}
`
	utilCode := `export function helper() {
  return "ok";
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "app.ts"), []byte(appCode), 0o644); err != nil {
		t.Fatalf("write app file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "util.ts"), []byte(utilCode), 0o644); err != nil {
		t.Fatalf("write util file: %v", err)
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "startApp",
		"repo":  tmpDir,
		"limit": 5,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}

	var foundDependency bool
	for _, result := range payload.Results {
		if result.Path == "util.ts" && containsString(result.Reasons, "graph_dependency") {
			foundDependency = true
			break
		}
	}
	if !foundDependency {
		t.Fatalf("expected util.ts graph dependency bridge, got %+v", payload.Results)
	}
}

func TestSearchToolAddsWorkingSetBoost(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}

	files := map[string]string{
		".gitignore": ".neurofs/\n",
		"alpha.go": `package service

func AlphaWorker() string {
	return "alpha"
}
`,
		"beta.go": `package service

func BetaWorker() string {
	return "beta"
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit(t, tmpDir, "init")
	runGit(t, tmpDir, "config", "user.email", "neurofs-test@example.com")
	runGit(t, tmpDir, "config", "user.name", "NeuroFS Test")
	runGit(t, tmpDir, "add", ".")
	runGit(t, tmpDir, "commit", "-m", "initial")

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	modifiedAlpha := `package service

func AlphaWorker() string {
	return "alpha changed"
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "alpha.go"), []byte(modifiedAlpha), 0o644); err != nil {
		t.Fatalf("modify alpha.go: %v", err)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "review current edits",
		"repo":  tmpDir,
		"limit": 5,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("expected working-set search result")
	}
	top := payload.Results[0]
	if top.Path != "alpha.go" {
		t.Fatalf("expected changed alpha.go first, got %+v", top)
	}
	if !containsString(top.Reasons, "working_set") {
		t.Fatalf("expected working_set reason, got %+v", top.Reasons)
	}
}

func TestSearchToolAddsExactContentBoostFromRG(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg executable not available")
	}

	goCode := `package service

func ExactNeedle() string {
	const NeedleID = "needle"
	return NeedleID
}

func OtherWorker() string {
	return NeedleIDValue
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "service.go"), []byte(goCode), 0o644); err != nil {
		t.Fatalf("write service.go: %v", err)
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "NeedleID",
		"repo":  tmpDir,
		"limit": 3,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("expected exact content result")
	}
	top := payload.Results[0]
	if top.Symbol != "ExactNeedle" {
		t.Fatalf("expected ExactNeedle first, got %+v", top)
	}
	if !containsString(top.Reasons, "exact_content") {
		t.Fatalf("expected exact_content reason, got %+v", top.Reasons)
	}
	for _, result := range payload.Results {
		if result.Symbol == "OtherWorker" && containsString(result.Reasons, "exact_content") {
			t.Fatalf("substring-only NeedleIDValue must not get exact_content: %+v", result)
		}
	}
}

func TestSearchToolAddsExactFilenameBoost(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	files := map[string]string{
		"auth.go": `package service

func Handler() string {
	return "ok"
}
`,
		"authenticator.go": `package service

func Handler() string {
	return "ok"
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "auth",
		"repo":  tmpDir,
		"limit": 2,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("expected exact filename result")
	}
	top := payload.Results[0]
	if top.Path != "auth.go" {
		t.Fatalf("expected auth.go first, got %+v", top)
	}
	if !containsString(top.Reasons, "exact_filename") {
		t.Fatalf("expected exact_filename reason, got %+v", top.Reasons)
	}
}

func TestSearchToolPenalizesLongChunksWhenSmallAlternativeExists(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	ctx := context.Background()
	tmpDir := t.TempDir()

	var goCode strings.Builder
	goCode.WriteString("package service\n\n")
	goCode.WriteString("func LargeMatch() string {\n")
	goCode.WriteString("\tvalue := \"TargetNeedle\"\n")
	for i := 0; i < 260; i++ {
		goCode.WriteString("\tvalue += \"filler filler filler filler filler\"\n")
	}
	goCode.WriteString("\treturn value\n")
	goCode.WriteString("}\n\n")
	goCode.WriteString("func SmallMatch() string {\n")
	goCode.WriteString("\treturn \"TargetNeedle\"\n")
	goCode.WriteString("}\n")

	if err := os.WriteFile(filepath.Join(tmpDir, "service.go"), []byte(goCode.String()), 0o644); err != nil {
		t.Fatalf("write service.go: %v", err)
	}

	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	searchArgsRaw, _ := json.Marshal(map[string]any{
		"query": "TargetNeedle",
		"repo":  tmpDir,
		"limit": 2,
	})
	searchRes := runSearchTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("search tool failed: %s", searchRes.Content[0].Text)
	}
	var payload searchResponse
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, searchRes.Content[0].Text)
	}
	if len(payload.Results) < 2 {
		t.Fatalf("expected two search results, got %+v", payload.Results)
	}
	if payload.Results[0].Symbol != "SmallMatch" {
		t.Fatalf("expected smaller chunk first, got %+v", payload.Results[0])
	}
	var foundPenalty bool
	for _, result := range payload.Results {
		if result.Symbol == "LargeMatch" && containsString(result.Reasons, "long_chunk_penalty") {
			foundPenalty = true
			break
		}
	}
	if !foundPenalty {
		t.Fatalf("expected LargeMatch long_chunk_penalty, got %+v", payload.Results)
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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
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

func TestSurgicalTools(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Write a mock TS file
	tsPath := "app.ts"
	tsCode := `
export function computeSum(a: number, b: number): number {
  return a + b;
}

export function unrelated() {
  console.log("hello");
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, tsPath), []byte(tsCode), 0o644); err != nil {
		t.Fatalf("write temp ts file: %v", err)
	}

	// 1) Scan the repo first using scan tool to populate DB
	scanArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	scanRes := runScanTool(ctx, scanArgsRaw)
	if scanRes.IsError {
		t.Fatalf("scan tool failed: %s", scanRes.Content[0].Text)
	}

	// 2) Test outline tool
	outlineArgsRaw, _ := json.Marshal(map[string]any{"repo": tmpDir})
	outlineRes := runGetOutlineTool(ctx, outlineArgsRaw)
	if outlineRes.IsError {
		t.Fatalf("outline tool failed: %s", outlineRes.Content[0].Text)
	}
	if !strings.Contains(outlineRes.Content[0].Text, tsPath) {
		t.Errorf("expected outline to list %q, got: %s", tsPath, outlineRes.Content[0].Text)
	}

	fileOutlineArgsRaw, _ := json.Marshal(map[string]any{
		"path": tsPath,
		"repo": tmpDir,
	})
	fileOutlineRes := runGetOutlineTool(ctx, fileOutlineArgsRaw)
	if fileOutlineRes.IsError {
		t.Fatalf("file outline tool failed: %s", fileOutlineRes.Content[0].Text)
	}
	var logic contextmap.LogicMap
	if err := json.Unmarshal([]byte(fileOutlineRes.Content[0].Text), &logic); err != nil {
		t.Fatalf("decode file outline: %v\n%s", err, fileOutlineRes.Content[0].Text)
	}
	if logic.Path != tsPath || len(logic.Symbols) == 0 {
		t.Fatalf("expected file logic map for %s, got %+v", tsPath, logic)
	}

	// 3) Test list signatures tool
	sigArgsRaw, _ := json.Marshal(map[string]any{
		"path": tsPath,
		"repo": tmpDir,
	})
	sigRes := runListSignaturesTool(ctx, sigArgsRaw)
	if sigRes.IsError {
		t.Fatalf("list signatures tool failed: %s", sigRes.Content[0].Text)
	}
	if !strings.Contains(sigRes.Content[0].Text, "computeSum") || !strings.Contains(sigRes.Content[0].Text, "unrelated") {
		t.Errorf("expected signatures to contain symbols, got: %s", sigRes.Content[0].Text)
	}

	// 4) Test get excerpt tool
	excArgsRaw, _ := json.Marshal(map[string]any{
		"path":  tsPath,
		"query": "sum",
		"repo":  tmpDir,
	})
	excRes := runGetExcerptTool(ctx, excArgsRaw)
	if excRes.IsError {
		t.Fatalf("get excerpt tool failed: %s", excRes.Content[0].Text)
	}
	if !strings.Contains(excRes.Content[0].Text, "computeSum") {
		t.Errorf("expected excerpt to contain matching function, got: %s", excRes.Content[0].Text)
	}
	if strings.Contains(excRes.Content[0].Text, "unrelated") {
		t.Errorf("expected excerpt to NOT contain unrelated function, got: %s", excRes.Content[0].Text)
	}

	taskArgsRaw, _ := json.Marshal(map[string]any{
		"query":      "change compute sum",
		"repo":       tmpDir,
		"budget":     2000,
		"agent":      true,
		"session_id": "mcp-task-session",
	})
	taskRes := runTaskTool(ctx, taskArgsRaw)
	if taskRes.IsError {
		t.Fatalf("task agent tool failed: %s", taskRes.Content[0].Text)
	}
	var taskPayload taskAgentResponse
	if err := json.Unmarshal([]byte(taskRes.Content[0].Text), &taskPayload); err != nil {
		t.Fatalf("decode task agent payload: %v\n%s", err, taskRes.Content[0].Text)
	}
	if taskPayload.SessionID != "mcp-task-session" || taskPayload.InitialTokens <= 0 || taskPayload.Prompt == "" || !taskPayload.ThinPrompt {
		t.Fatalf("unexpected task agent payload: %+v", taskPayload)
	}
	if len(taskPayload.NextActions) == 0 {
		t.Fatalf("expected next actions in task agent payload: %+v", taskPayload)
	}
	if !strings.Contains(taskPayload.Prompt, "<patch_context session=\"mcp-task-session\">") ||
		!strings.Contains(taskPayload.Prompt, "call neurofs_expand") ||
		!strings.Contains(taskPayload.Prompt, "call neurofs_measure") {
		t.Fatalf("expected MCP patch context in task prompt:\n%s", taskPayload.Prompt)
	}
	if strings.Contains(taskPayload.Prompt, `return a + b`) {
		t.Fatalf("MCP agent prompt should be thin and avoid eager source bodies:\n%s", taskPayload.Prompt)
	}
	taskMeasureArgsRaw, _ := json.Marshal(map[string]any{
		"repo":       tmpDir,
		"session_id": "mcp-task-session",
	})
	taskMeasureRes := runMeasureTool(ctx, taskMeasureArgsRaw)
	if taskMeasureRes.IsError {
		t.Fatalf("task measure failed: %s", taskMeasureRes.Content[0].Text)
	}
	var taskSummary contextusage.Summary
	if err := json.Unmarshal([]byte(taskMeasureRes.Content[0].Text), &taskSummary); err != nil {
		t.Fatalf("decode task measure summary: %v\n%s", err, taskMeasureRes.Content[0].Text)
	}
	if taskSummary.InitialTokens != taskPayload.InitialTokens || taskSummary.Expansions != 0 {
		t.Fatalf("unexpected task measure summary: %+v payload=%+v", taskSummary, taskPayload)
	}

	sessionID := "mcp-session"
	if err := contextusage.Append(tmpDir, contextusage.Entry{
		SessionID: sessionID,
		Phase:     "initial_bundle",
		Command:   "test",
		Tokens:    11,
	}); err != nil {
		t.Fatalf("append usage: %v", err)
	}
	expandArgsRaw, _ := json.Marshal(map[string]any{
		"target":     tsPath + ":2-4",
		"repo":       tmpDir,
		"session_id": sessionID,
	})
	expandRes := runExpandTool(ctx, expandArgsRaw)
	if expandRes.IsError {
		t.Fatalf("expand tool failed: %s", expandRes.Content[0].Text)
	}
	var expanded contextladder.ExpandedContent
	if err := json.Unmarshal([]byte(expandRes.Content[0].Text), &expanded); err != nil {
		t.Fatalf("decode expanded content: %v\n%s", err, expandRes.Content[0].Text)
	}
	if expanded.Mode != contextladder.ModeExcerpt || expanded.Path != tsPath || !strings.Contains(expanded.Content, "computeSum") {
		t.Fatalf("unexpected expand payload: %+v", expanded)
	}
	if strings.Contains(expanded.Content, "unrelated") {
		t.Fatalf("expand should only return requested range, got %+v", expanded)
	}

	measureArgsRaw, _ := json.Marshal(map[string]any{
		"repo":       tmpDir,
		"session_id": sessionID,
	})
	measureRes := runMeasureTool(ctx, measureArgsRaw)
	if measureRes.IsError {
		t.Fatalf("measure tool failed: %s", measureRes.Content[0].Text)
	}
	var summary contextusage.Summary
	if err := json.Unmarshal([]byte(measureRes.Content[0].Text), &summary); err != nil {
		t.Fatalf("decode measure summary: %v\n%s", err, measureRes.Content[0].Text)
	}
	if summary.InitialTokens != 11 || summary.Expansions != 1 || summary.TotalTokens <= summary.InitialTokens {
		t.Fatalf("unexpected measure summary: %+v", summary)
	}
}

func traceHasTool(trace []ContextTraceStep, tool string) bool {
	for _, step := range trace {
		if step.Tool == tool {
			return true
		}
	}
	return false
}

func traceReasonContains(trace []ContextTraceStep, text string) bool {
	for _, step := range trace {
		if strings.Contains(step.Reason, text) {
			return true
		}
	}
	return false
}

func TestMemoryTools(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Log an entry using the log tool (generic/active session)
	logArgsRaw, _ := json.Marshal(map[string]any{
		"query":   "search something",
		"command": "go run .",
		"outcome": "success",
		"notes":   "verified memory tool",
		"repo":    tmpDir,
	})
	logRes := runLogMemoryTool(ctx, logArgsRaw)
	if logRes.IsError {
		t.Fatalf("expected log memory tool to succeed, got error: %s", logRes.Content[0].Text)
	}
	if !strings.Contains(logRes.Content[0].Text, "Successfully logged entry to session") {
		t.Errorf("expected success message, got: %q", logRes.Content[0].Text)
	}

	// 2. Search for the entry using the search tool with "term"
	searchArgsRaw, _ := json.Marshal(map[string]any{
		"term": "verified",
		"repo": tmpDir,
	})
	searchRes := runSearchMemoryTool(ctx, searchArgsRaw)
	if searchRes.IsError {
		t.Fatalf("expected search memory tool to succeed, got error: %s", searchRes.Content[0].Text)
	}

	var results []models.LedgerEntry
	if err := json.Unmarshal([]byte(searchRes.Content[0].Text), &results); err != nil {
		t.Fatalf("failed to parse search results: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Notes != "verified memory tool" {
		t.Errorf("expected notes to be 'verified memory tool', got %q", results[0].Notes)
	}
	if results[0].Command != "go run ." {
		t.Errorf("expected command to be 'go run .', got %q", results[0].Command)
	}

	// 3. Log a manual entry with a custom session ID and files array
	customLogArgs, _ := json.Marshal(map[string]any{
		"query":      "implement feature",
		"command":    "go build",
		"outcome":    "success",
		"notes":      "verified custom session details",
		"session_id": "sess-custom-1",
		"files":      []string{"main.go", "helper.go"},
		"repo":       tmpDir,
	})
	logRes2 := runLogMemoryTool(ctx, customLogArgs)
	if logRes2.IsError {
		t.Fatalf("expected custom log memory tool to succeed, got error: %s", logRes2.Content[0].Text)
	}
	if !strings.Contains(logRes2.Content[0].Text, "sess-custom-1") {
		t.Errorf("expected success message with session ID, got: %q", logRes2.Content[0].Text)
	}

	// 4. Search using custom session ID filter
	customSearchArgs, _ := json.Marshal(map[string]any{
		"term":       "custom",
		"session_id": "sess-custom-1",
		"repo":       tmpDir,
	})
	searchRes2 := runSearchMemoryTool(ctx, customSearchArgs)
	if searchRes2.IsError {
		t.Fatalf("expected search memory tool with session ID to succeed, got error: %s", searchRes2.Content[0].Text)
	}

	var results2 []models.LedgerEntry
	if err := json.Unmarshal([]byte(searchRes2.Content[0].Text), &results2); err != nil {
		t.Fatalf("failed to parse search results: %v", err)
	}
	if len(results2) != 1 {
		t.Fatalf("expected 1 custom session result, got %d", len(results2))
	}
	if results2[0].SessionID != "sess-custom-1" {
		t.Errorf("expected session ID to be 'sess-custom-1', got %q", results2[0].SessionID)
	}
	if len(results2[0].Files) != 2 || results2[0].Files[0] != "main.go" {
		t.Errorf("expected files array, got %+v", results2[0].Files)
	}

	// 5. Export memory for the custom session format="session_timeline"
	exportArgs, _ := json.Marshal(map[string]any{
		"format":     "session_timeline",
		"session_id": "sess-custom-1",
		"repo":       tmpDir,
	})
	exportRes := runExportMemoryTool(ctx, exportArgs)
	if exportRes.IsError {
		t.Fatalf("expected export memory tool to succeed, got error: %s", exportRes.Content[0].Text)
	}
	exportText := exportRes.Content[0].Text
	if !strings.Contains(exportText, "sess-custom-1") {
		t.Errorf("expected export to contain session ID, got:\n%s", exportText)
	}
	if !strings.Contains(exportText, "main.go") || !strings.Contains(exportText, "helper.go") {
		t.Errorf("expected export to contain file names, got:\n%s", exportText)
	}

	// 6. Export memory for the custom session format="agents"
	exportArgsAgents, _ := json.Marshal(map[string]any{
		"format":     "agents",
		"session_id": "sess-custom-1",
		"repo":       tmpDir,
	})
	exportResAgents := runExportMemoryTool(ctx, exportArgsAgents)
	if exportResAgents.IsError {
		t.Fatalf("expected agents export memory tool to succeed, got error: %s", exportResAgents.Content[0].Text)
	}
	exportTextAgents := exportResAgents.Content[0].Text
	if !strings.Contains(exportTextAgents, "Agent Handoff Context") {
		t.Errorf("expected agents header, got:\n%s", exportTextAgents)
	}

	// 7. Export memory for the custom session format="markdown"
	exportArgsMD, _ := json.Marshal(map[string]any{
		"format":     "markdown",
		"session_id": "sess-custom-1",
		"repo":       tmpDir,
	})
	exportResMD := runExportMemoryTool(ctx, exportArgsMD)
	if exportResMD.IsError {
		t.Fatalf("expected markdown export memory tool to succeed, got error: %s", exportResMD.Content[0].Text)
	}
	exportTextMD := exportResMD.Content[0].Text
	if !strings.Contains(exportTextMD, "Session Ledger Log") {
		t.Errorf("expected markdown header, got:\n%s", exportTextMD)
	}
}

func TestPruneMemoryTool(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Touch last_prune_sqlite.txt to prevent background auto-prune during test appends
	pruneFileSql := filepath.Join(tmpDir, ".neurofs", "last_prune_sqlite.txt")
	_ = os.MkdirAll(filepath.Dir(pruneFileSql), 0755)
	_ = os.WriteFile(pruneFileSql, []byte(time.Now().Format(time.RFC3339)), 0644)

	// 1. Log an entry that will be pruned (older than 30 days)
	m := memory.New(memory.NewSqliteStore(tmpDir))
	err := m.AppendEntry(ctx, models.LedgerEntry{
		Query:     "old query",
		Timestamp: time.Now().Add(-60 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Log an entry that will be kept (recent)
	err = m.AppendEntry(ctx, models.LedgerEntry{
		Query:     "new query",
		Timestamp: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Call neurofs_prune_memory tool
	pruneArgsRaw, _ := json.Marshal(map[string]any{
		"days": 30,
		"repo": tmpDir,
	})
	pruneRes := runPruneMemoryTool(ctx, pruneArgsRaw)
	if pruneRes.IsError {
		t.Fatalf("expected prune memory tool to succeed, got error: %s", pruneRes.Content[0].Text)
	}
	if !strings.Contains(pruneRes.Content[0].Text, "Successfully pruned 1 entries") {
		t.Errorf("expected pruned message, got: %q", pruneRes.Content[0].Text)
	}

	// 4. Verify sqlite contents
	entries, err := memory.NewSqliteStore(tmpDir).Read(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Query != "new query" {
		t.Errorf("expected 1 entry left ('new query'), got %d entries: %+v", len(entries), entries)
	}
}
