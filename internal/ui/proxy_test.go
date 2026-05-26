package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/storage"
)

func TestExtractUserText(t *testing.T) {
	// Case 1: Simple string content
	msg1 := anthropicMessage{
		Role:    "user",
		Content: json.RawMessage(`"hello world"`),
	}
	got1 := extractUserText(msg1)
	if got1 != "hello world" {
		t.Errorf("got %q, want %q", got1, "hello world")
	}

	// Case 2: Array of content blocks
	msg2 := anthropicMessage{
		Role:    "user",
		Content: json.RawMessage(`[{"type": "text", "text": "hello"}, {"type": "text", "text": "world"}]`),
	}
	got2 := extractUserText(msg2)
	if got2 != "hello\nworld" {
		t.Errorf("got %q, want %q", got2, "hello\nworld")
	}

	// Case 3: Mixed content blocks (non-text should be ignored)
	msg3 := anthropicMessage{
		Role:    "user",
		Content: json.RawMessage(`[{"type": "image", "source": "..."}, {"type": "text", "text": "hello"}]`),
	}
	got3 := extractUserText(msg3)
	if got3 != "hello" {
		t.Errorf("got %q, want %q", got3, "hello")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	contextBundle := "<context>Some context here</context>"

	// Case 1: Empty system prompt
	got1 := buildSystemPrompt(nil, contextBundle)
	var blocks1 []map[string]any
	if err := json.Unmarshal(got1, &blocks1); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(blocks1) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks1))
	}
	if blocks1[0]["type"] != "text" || blocks1[0]["text"] != contextBundle {
		t.Errorf("unexpected block content: %+v", blocks1[0])
	}
	cc1, ok := blocks1[0]["cache_control"].(map[string]any)
	if !ok || cc1["type"] != "ephemeral" {
		t.Errorf("expected cache_control ephemeral, got: %+v", blocks1[0]["cache_control"])
	}

	// Case 2: Simple string system prompt
	existing2 := json.RawMessage(`"Original system prompt"`)
	got2 := buildSystemPrompt(existing2, contextBundle)
	var blocks2 []map[string]any
	if err := json.Unmarshal(got2, &blocks2); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(blocks2) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks2))
	}
	if blocks2[0]["text"] != contextBundle || blocks2[1]["text"] != "Original system prompt" {
		t.Errorf("unexpected blocks: %+v", blocks2)
	}
	cc2, ok := blocks2[0]["cache_control"].(map[string]any)
	if !ok || cc2["type"] != "ephemeral" {
		t.Errorf("expected cache_control, got: %+v", blocks2[0]["cache_control"])
	}

	// Case 3: Block array system prompt
	existing3 := json.RawMessage(`[{"type": "text", "text": "Original text block"}]`)
	got3 := buildSystemPrompt(existing3, contextBundle)
	var blocks3 []map[string]any
	if err := json.Unmarshal(got3, &blocks3); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(blocks3) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks3))
	}
	if blocks3[0]["text"] != contextBundle || blocks3[1]["text"] != "Original text block" {
		t.Errorf("unexpected blocks: %+v", blocks3)
	}
	cc3, ok := blocks3[0]["cache_control"].(map[string]any)
	if !ok || cc3["type"] != "ephemeral" {
		t.Errorf("expected cache_control, got: %+v", blocks3[0]["cache_control"])
	}
}

func TestProxyLoggingAndStats(t *testing.T) {
	// Reset variables
	proxyLogMu.Lock()
	totalReqs = 0
	totalSaved = 0
	totalSavedUSD = 0.0
	recentLogs = nil
	proxyLogMu.Unlock()

	logProxyRequest("claude-test", "how does ranking work?", 10000, 2000)

	proxyLogMu.RLock()
	defer proxyLogMu.RUnlock()

	if totalReqs != 1 {
		t.Errorf("expected 1 request, got %d", totalReqs)
	}
	if totalSaved != 8000 {
		t.Errorf("expected 8000 tokens saved, got %d", totalSaved)
	}
	expectedUSD := 8000.0 * 3.00 / 1000000.0
	if totalSavedUSD != expectedUSD {
		t.Errorf("expected %f USD saved, got %f", expectedUSD, totalSavedUSD)
	}
	if len(recentLogs) != 1 {
		t.Errorf("expected 1 log in slice, got %d", len(recentLogs))
	}
	if recentLogs[0].Model != "claude-test" || recentLogs[0].Query != "how does ranking work?" {
		t.Errorf("unexpected log entries: %+v", recentLogs[0])
	}
}

func TestHandleProxyStats(t *testing.T) {
	// Reset variables
	proxyLogMu.Lock()
	totalReqs = 0
	totalSaved = 0
	totalSavedUSD = 0.0
	recentLogs = nil
	proxyLogMu.Unlock()

	logProxyRequest("claude-test-stats", "test query", 5000, 1000)

	req := httptest.NewRequest("GET", "/api/proxy/stats", nil)
	rr := httptest.NewRecorder()

	handleProxyStats(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rr.Code)
	}

	var stats ProxyStats
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("failed to decode stats response: %v", err)
	}

	if stats.TotalRequests != 1 {
		t.Errorf("expected 1 request, got %d", stats.TotalRequests)
	}
	if stats.TotalSaved != 4000 {
		t.Errorf("expected 4000 tokens saved, got %d", stats.TotalSaved)
	}
	if len(stats.RecentLogs) != 1 || stats.RecentLogs[0].Model != "claude-test-stats" {
		t.Errorf("unexpected recent logs in stats: %+v", stats.RecentLogs)
	}
}

func TestHandleProxyStatsWithDB(t *testing.T) {
	tempDir := t.TempDir()

	dbPath := filepath.Join(tempDir, ".neurofs", "index.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	now := time.Now()
	err = db.InsertProxyLog(now, "claude-db-test", "query db", 8000, 2000, 6000, 0.018)
	if err != nil {
		db.Close()
		t.Fatalf("failed to insert proxy log: %v", err)
	}
	db.Close()

	req := httptest.NewRequest("GET", "/api/proxy/stats?repo="+tempDir, nil)
	rr := httptest.NewRecorder()

	handleProxyStats(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rr.Code)
	}

	var stats ProxyStats
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("failed to decode stats response: %v", err)
	}

	if stats.TotalRequests != 1 {
		t.Errorf("expected 1 request, got %d", stats.TotalRequests)
	}
	if stats.TotalSaved != 6000 {
		t.Errorf("expected 6000 tokens saved, got %d", stats.TotalSaved)
	}
	if stats.TotalSavedUSD != 0.018 {
		t.Errorf("expected 0.018 USD saved, got %f", stats.TotalSavedUSD)
	}
	if len(stats.RecentLogs) != 1 || stats.RecentLogs[0].Model != "claude-db-test" {
		t.Errorf("unexpected logs: %+v", stats.RecentLogs)
	}
}

func TestExtractOpenAIText(t *testing.T) {
	// Case 1: Simple string
	msg1 := openAIMessage{
		Role:    "user",
		Content: json.RawMessage(`"hello world"`),
	}
	got1 := extractOpenAIText(msg1)
	if got1 != "hello world" {
		t.Errorf("got %q, want %q", got1, "hello world")
	}

	// Case 2: Array of content blocks
	msg2 := openAIMessage{
		Role:    "user",
		Content: json.RawMessage(`[{"type": "text", "text": "hello"}, {"type": "text", "text": "world"}]`),
	}
	got2 := extractOpenAIText(msg2)
	if got2 != "hello\nworld" {
		t.Errorf("got %q, want %q", got2, "hello\nworld")
	}
}

func TestProxyLoggingGPT(t *testing.T) {
	proxyLogMu.Lock()
	totalReqs = 0
	totalSaved = 0
	totalSavedUSD = 0.0
	recentLogs = nil
	proxyLogMu.Unlock()

	logProxyRequest("gpt-4o", "some question", 10000, 2000)

	proxyLogMu.RLock()
	defer proxyLogMu.RUnlock()

	if totalReqs != 1 {
		t.Errorf("expected 1 request, got %d", totalReqs)
	}
	if totalSaved != 8000 {
		t.Errorf("expected 8000 tokens saved, got %d", totalSaved)
	}
	expectedUSD := 8000.0 * 2.50 / 1000000.0
	if totalSavedUSD != expectedUSD {
		t.Errorf("expected %f USD saved, got %f", expectedUSD, totalSavedUSD)
	}
}

func TestHandleChatAuthError(t *testing.T) {
	os.Setenv("ANTHROPIC_API_KEY", "")
	os.Setenv("OPENAI_API_KEY", "")

	tempDir := t.TempDir()
	dbDir := filepath.Join(tempDir, ".neurofs")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("failed to create db dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "index.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	err = db.UpsertFile(models.FileRecord{
		Path:      filepath.Join(tempDir, "main.go"),
		RelPath:   "main.go",
		Checksum:  "abc",
		Size:      30,
		Lines:     3,
		IndexedAt: time.Now(),
	})
	if err != nil {
		db.Close()
		t.Fatalf("failed to save dummy file: %v", err)
	}
	db.Close()

	reqBody := `{"repo":"` + tempDir + `","provider":"anthropic","model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()

	handleChat(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", rr.Code)
	}
}

func TestHandleChatStreamSimulation(t *testing.T) {
	// Start local mock server for upstream
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Mock response\"}}\n\n"))
	}))
	defer mockUpstream.Close()

	os.Setenv("NEUROFS_TEST_ANTHROPIC_URL", mockUpstream.URL)
	defer os.Setenv("NEUROFS_TEST_ANTHROPIC_URL", "")

	os.Setenv("ANTHROPIC_API_KEY", "test-key")
	defer os.Setenv("ANTHROPIC_API_KEY", "")

	tempDir := t.TempDir()
	dbDir := filepath.Join(tempDir, ".neurofs")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("failed to create db dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "index.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	err = db.UpsertFile(models.FileRecord{
		Path:      filepath.Join(tempDir, "main.go"),
		RelPath:   "main.go",
		Checksum:  "abc",
		Size:      30,
		Lines:     3,
		IndexedAt: time.Now(),
	})
	if err != nil {
		db.Close()
		t.Fatalf("failed to save dummy file: %v", err)
	}
	db.Close()

	reqBody := `{"repo":"` + tempDir + `","provider":"anthropic","model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()

	handleChat(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	if !strings.Contains(body, "event: neurofs_metadata") {
		t.Errorf("expected body to contain event: neurofs_metadata, got: %s", body)
	}
	if !strings.Contains(body, "Mock response") {
		t.Errorf("expected body to contain Mock response, got: %s", body)
	}
}


