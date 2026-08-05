package ui

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"context"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/output"
	"github.com/Gere2/neurofs/internal/packager"
	"github.com/Gere2/neurofs/internal/ranking"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/Gere2/neurofs/internal/taskflow"
)

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicReq struct {
	Model       string             `json:"model"`
	Messages    []anthropicMessage `json:"messages"`
	System      json.RawMessage    `json:"system,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type openAIChatReq struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

const maxUpstreamErrorBodyBytes = int64(1 << 20)

func closeProxyDB(db *storage.DB, operation string) {
	if err := db.Close(); err != nil {
		log.Printf("neurofs proxy: close database after %s: %v", operation, err)
	}
}

func extractOpenAIText(m openAIMessage) string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// handleProxyMessages acts as a transparent proxy for the Anthropic Messages API.
// It intercepts the client prompt, retrieves relevant repository context using NeuroFS,
// injects the XML context bundle into the Anthropic payload, and forwards the request.
func handleProxyMessages(w http.ResponseWriter, r *http.Request) {
	// 1. Read and decode the client payload
	bodyBytes, ok := readLimitedBody(w, r)
	if !ok {
		return
	}

	var payload anthropicReq
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		// If it doesn't parse as an Anthropic request, just forward the raw bytes to Anthropic
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	// 2. Extract the last user message to use as a search query
	var query string
	for i := len(payload.Messages) - 1; i >= 0; i-- {
		if payload.Messages[i].Role == "user" {
			query = extractUserText(payload.Messages[i])
			break
		}
	}

	// If no query is found, just forward the raw request
	if strings.TrimSpace(query) == "" {
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	// 3. Gather repository context
	repoDir := startupDir
	if sandboxActive && pinnedRepo != "" {
		repoDir = pinnedRepo
	}
	if repoDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			repoDir = cwd
		}
	}

	cfg, err := config.New(repoDir)
	if err != nil {
		// Non-fatal: if we can't initialize config, just log/skip and forward raw request
		fmt.Fprintf(os.Stderr, "proxy: failed to initialize config at %s: %v\n", repoDir, err)
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	// Auto-scan if the database is missing or empty. Also, schedule a background scan check.
	dbPath := cfg.DBPath
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		db, err := storage.Open(dbPath)
		if err == nil {
			_, _ = indexer.Run(cfg, db, indexer.Options{})
			if err := db.Close(); err != nil {
				log.Printf("neurofs proxy: close auto-scan database: %v", err)
			}
		}
	} else {
		triggerBackgroundScan(cfg)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: failed to open database at %s: %v\n", dbPath, err)
		forwardRawRequest(w, r, bodyBytes)
		return
	}
	defer closeProxyDB(db, "Anthropic request")

	files, err := db.AllFiles()
	if err != nil || len(files) == 0 {
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	// Get query embedding and all file embeddings
	embClient := embeddings.NewClient(cfg.HybridMode)
	queryEmb, _ := embClient.GetEmbedding(context.Background(), query)
	fileEmbs, _ := db.AllEmbeddings()
	rels, _ := db.AllRelations()

	// 4. Rank and package the context bundle
	rankWeights, _, _ := ranking.LoadWeights(cfg.RepoRoot)
	rankOpts := ranking.Options{
		Project:        taskflow.LoadProjectInfo(db),
		ChangedFiles:   gitChangedFiles(cfg.RepoRoot),
		QueryEmbedding: queryEmb,
		Embeddings:     fileEmbs,
		Relations:      rels,
		Weights:        &rankWeights,
	}
	ranked := ranking.RankWithOptions(files, query, rankOpts)

	budget := config.DefaultBudget
	bundle, err := packager.Pack(ranked, query, packager.Options{
		Budget:           budget,
		UpgradeWithSlack: true,
		QueryTerms:       ranking.Tokenise(query),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: pack error: %v\n", err)
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	var promptBuf bytes.Buffer
	if err := output.WriteClaude(&promptBuf, bundle, taskflow.BuildRepoSummary(cfg.RepoRoot, files, taskflow.LoadProjectInfo(db))); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: render prompt error: %v\n", err)
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	// 5. Inject the context bundle into the system field
	payload.System = buildSystemPrompt(payload.System, promptBuf.String())

	// Log proxy request statistics
	totalBytes := int64(0)
	for _, f := range files {
		totalBytes += f.Size
	}
	before := int(totalBytes / 4)
	after := bundle.Stats.TokensUsed
	saved := before - after
	if saved < 0 {
		saved = 0
	}
	usd := float64(saved) * 3.00 / 1000000.0
	_ = db.InsertProxyLog(time.Now(), payload.Model, query, before, after, saved, usd)

	logProxyRequest(payload.Model, query, before, after)

	// 6. Marshal the updated payload back to JSON
	newBodyBytes, err := replaceJSONRawField(bodyBytes, "system", payload.System)
	if err != nil {
		forwardRawRequest(w, r, bodyBytes)
		return
	}

	// 7. Forward the modified request to Anthropic
	forwardRawRequest(w, r, newBodyBytes)
}

// handleProxyOpenAIMessages acts as a transparent proxy for OpenAI Chat Completions API.
// It intercepts the client prompt, retrieves repository context using NeuroFS,
// injects the XML context bundle as a system message, and forwards the request.
func handleProxyOpenAIMessages(w http.ResponseWriter, r *http.Request) {
	// 1. Read and decode the client payload
	bodyBytes, ok := readLimitedBody(w, r)
	if !ok {
		return
	}

	var payload openAIChatReq
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		// If it doesn't parse as an OpenAI request, just forward the raw bytes to OpenAI
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	// 2. Extract the last user message to use as a search query
	var query string
	for i := len(payload.Messages) - 1; i >= 0; i-- {
		if payload.Messages[i].Role == "user" {
			query = extractOpenAIText(payload.Messages[i])
			break
		}
	}

	// If no query is found, just forward the raw request
	if strings.TrimSpace(query) == "" {
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	// 3. Gather repository context
	repoDir := startupDir
	if sandboxActive && pinnedRepo != "" {
		repoDir = pinnedRepo
	}
	if repoDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			repoDir = cwd
		}
	}

	cfg, err := config.New(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: failed to initialize config at %s: %v\n", repoDir, err)
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	// Auto-scan if the database is missing or empty. Also, schedule a background scan check.
	dbPath := cfg.DBPath
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		db, err := storage.Open(dbPath)
		if err == nil {
			_, _ = indexer.Run(cfg, db, indexer.Options{})
			closeProxyDB(db, "OpenAI auto-scan")
		}
	} else {
		triggerBackgroundScan(cfg)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: failed to open database at %s: %v\n", dbPath, err)
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}
	defer closeProxyDB(db, "OpenAI request")

	files, err := db.AllFiles()
	if err != nil || len(files) == 0 {
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	// Get query embedding and all file embeddings
	embClient := embeddings.NewClient(cfg.HybridMode)
	queryEmb, _ := embClient.GetEmbedding(context.Background(), query)
	fileEmbs, _ := db.AllEmbeddings()
	rels, _ := db.AllRelations()

	// 4. Rank and package the context bundle
	rankWeights, _, _ := ranking.LoadWeights(cfg.RepoRoot)
	rankOpts := ranking.Options{
		Project:        taskflow.LoadProjectInfo(db),
		ChangedFiles:   gitChangedFiles(cfg.RepoRoot),
		QueryEmbedding: queryEmb,
		Embeddings:     fileEmbs,
		Relations:      rels,
		Weights:        &rankWeights,
	}
	ranked := ranking.RankWithOptions(files, query, rankOpts)

	budget := config.DefaultBudget
	bundle, err := packager.Pack(ranked, query, packager.Options{
		Budget:           budget,
		UpgradeWithSlack: true,
		QueryTerms:       ranking.Tokenise(query),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: pack error: %v\n", err)
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	var promptBuf bytes.Buffer
	if err := output.WriteClaude(&promptBuf, bundle, taskflow.BuildRepoSummary(cfg.RepoRoot, files, taskflow.LoadProjectInfo(db))); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: render prompt error: %v\n", err)
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	// 5. Inject the context bundle as a system message at the beginning of messages
	contextText := promptBuf.String()
	contextJSON, _ := json.Marshal(contextText)
	systemMsg := openAIMessage{
		Role:    "system",
		Content: contextJSON,
	}
	payload.Messages = append([]openAIMessage{systemMsg}, payload.Messages...)

	// Log proxy request statistics to DB and in-memory
	totalBytes := int64(0)
	for _, f := range files {
		totalBytes += f.Size
	}
	before := int(totalBytes / 4)
	after := bundle.Stats.TokensUsed
	saved := before - after
	if saved < 0 {
		saved = 0
	}
	// GPT-4o input tokens cost $2.50 per million
	usd := float64(saved) * 2.50 / 1000000.0
	_ = db.InsertProxyLog(time.Now(), payload.Model, query, before, after, saved, usd)

	logProxyRequest(payload.Model, query, before, after)

	// 6. Marshal the updated payload back to JSON
	newBodyBytes, err := prependOpenAIMessage(bodyBytes, systemMsg)
	if err != nil {
		forwardOpenAIRequest(w, r, bodyBytes)
		return
	}

	// 7. Forward the modified request to OpenAI
	forwardOpenAIRequest(w, r, newBodyBytes)
}

func forwardOpenAIRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create upstream request: "+err.Error())
		return
	}

	// Copy original request headers
	for k, vv := range r.Header {
		if strings.EqualFold(k, "host") ||
			strings.EqualFold(k, "content-length") ||
			strings.EqualFold(k, remoteTokenHeader) {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("neurofs proxy: close OpenAI upstream response: %v", err)
		}
	}()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if ok {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

var (
	lastAutoScan time.Time
	scanMu       sync.Mutex
)

func triggerBackgroundScan(cfg *config.Config) {
	if flag.Lookup("test.v") != nil {
		return
	}
	scanMu.Lock()
	defer scanMu.Unlock()

	// Only trigger background scan at most once every 10 seconds
	if time.Since(lastAutoScan) < 10*time.Second {
		return
	}
	lastAutoScan = time.Now()

	go func() {
		db, err := storage.Open(cfg.DBPath)
		if err != nil {
			return
		}
		defer closeProxyDB(db, "background scan")
		_, _ = indexer.Run(cfg, db, indexer.Options{})
	}()
}

func extractUserText(m anthropicMessage) string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

func replaceJSONRawField(body []byte, field string, value json.RawMessage) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("request must be a JSON object")
	}
	root[field] = append(json.RawMessage(nil), value...)
	return json.Marshal(root)
}

func prependOpenAIMessage(body []byte, message openAIMessage) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("request must be a JSON object")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(root["messages"], &messages); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	rawMessage, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	messages = append([]json.RawMessage{rawMessage}, messages...)
	rawMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	root["messages"] = rawMessages
	return json.Marshal(root)
}

func buildSystemPrompt(existingSystem json.RawMessage, contextBundle string) json.RawMessage {
	// Build the NeuroFS block with cache control
	neuroFSBlock := map[string]any{
		"type": "text",
		"text": contextBundle,
		"cache_control": map[string]string{
			"type": "ephemeral",
		},
	}

	if len(existingSystem) == 0 {
		blocks := []map[string]any{neuroFSBlock}
		b, _ := json.Marshal(blocks)
		return b
	}

	var sysStr string
	if err := json.Unmarshal(existingSystem, &sysStr); err == nil {
		originalBlock := map[string]any{
			"type": "text",
			"text": sysStr,
		}
		blocks := []map[string]any{neuroFSBlock, originalBlock}
		b, _ := json.Marshal(blocks)
		return b
	}

	var blocks []map[string]any
	if err := json.Unmarshal(existingSystem, &blocks); err == nil {
		blocks = append([]map[string]any{neuroFSBlock}, blocks...)
		b, _ := json.Marshal(blocks)
		return b
	}

	// Fallback to block array if unmarshal fails
	fallbackBlocks := []map[string]any{neuroFSBlock}
	b, _ := json.Marshal(fallbackBlocks)
	return b
}

func forwardRawRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create upstream request: "+err.Error())
		return
	}

	// Copy original request headers
	for k, vv := range r.Header {
		if strings.EqualFold(k, "host") ||
			strings.EqualFold(k, "content-length") ||
			strings.EqualFold(k, remoteTokenHeader) {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("neurofs proxy: close Anthropic upstream response: %v", err)
		}
	}()

	// Copy response headers back
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream/copy response body
	flusher, ok := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if ok {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

type ProxyLog struct {
	Timestamp    time.Time `json:"timestamp"`
	Model        string    `json:"model"`
	Query        string    `json:"query"`
	TokensBefore int       `json:"tokens_before"`
	TokensAfter  int       `json:"tokens_after"`
	SavedTokens  int       `json:"saved_tokens"`
	SavingsUSD   float64   `json:"savings_usd"`
}

type ProxyStats struct {
	TotalRequests int        `json:"total_requests"`
	TotalSaved    int        `json:"total_saved_tokens"`
	TotalSavedUSD float64    `json:"total_saved_usd"`
	RecentLogs    []ProxyLog `json:"recent_logs"`
}

var (
	proxyLogMu    sync.RWMutex
	totalReqs     int
	totalSaved    int
	totalSavedUSD float64
	recentLogs    []ProxyLog
)

func logProxyRequest(model, query string, before, after int) {
	proxyLogMu.Lock()
	defer proxyLogMu.Unlock()

	saved := before - after
	if saved < 0 {
		saved = 0
	}
	rate := 3.00 // Default Anthropic
	if strings.Contains(strings.ToLower(model), "gpt") {
		rate = 2.50
	}
	usd := float64(saved) * rate / 1000000.0

	totalReqs++
	totalSaved += saved
	totalSavedUSD += usd

	log := ProxyLog{
		Timestamp:    time.Now(),
		Model:        model,
		Query:        query,
		TokensBefore: before,
		TokensAfter:  after,
		SavedTokens:  saved,
		SavingsUSD:   usd,
	}

	recentLogs = append([]ProxyLog{log}, recentLogs...)
	if len(recentLogs) > 100 {
		recentLogs = recentLogs[:100]
	}
}

func handleProxyStats(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo != "" {
		if sandboxActive && pinnedRepo != "" {
			absRepo, err := filepath.Abs(repo)
			if err != nil || !sameDir(absRepo, pinnedRepo) {
				writeErr(w, http.StatusForbidden, fmt.Sprintf("access denied: server is sandboxed to %q", pinnedRepo))
				return
			}
		}
		cfg, err := config.New(repo)
		if err == nil {
			if err := cfg.Validate(); err == nil {
				db, err := storage.Open(cfg.DBPath)
				if err == nil {
					defer closeProxyDB(db, "stats request")
					count, saved, usd, err := db.GetProxySummary()
					if err == nil {
						dbLogs, err := db.GetProxyLogs(100)
						if err == nil {
							logs := make([]ProxyLog, len(dbLogs))
							for i, l := range dbLogs {
								logs[i] = ProxyLog{
									Timestamp:    l.Timestamp,
									Model:        l.Model,
									Query:        l.Query,
									TokensBefore: l.TokensBefore,
									TokensAfter:  l.TokensAfter,
									SavedTokens:  l.SavedTokens,
									SavingsUSD:   l.SavingsUSD,
								}
							}
							stats := ProxyStats{
								TotalRequests: count,
								TotalSaved:    saved,
								TotalSavedUSD: usd,
								RecentLogs:    logs,
							}
							writeJSON(w, http.StatusOK, stats)
							return
						}
					}
				}
			}
		}
	}

	proxyLogMu.RLock()
	defer proxyLogMu.RUnlock()

	stats := ProxyStats{
		TotalRequests: totalReqs,
		TotalSaved:    totalSaved,
		TotalSavedUSD: totalSavedUSD,
		RecentLogs:    recentLogs,
	}
	if stats.RecentLogs == nil {
		stats.RecentLogs = []ProxyLog{}
	}
	writeJSON(w, http.StatusOK, stats)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Repo             string        `json:"repo"`
	Provider         string        `json:"provider"` // "anthropic" | "openai"
	Model            string        `json:"model"`
	Messages         []chatMessage `json:"messages"`
	Budget           int           `json:"budget"`
	Focus            string        `json:"focus"`
	Changed          bool          `json:"changed"`
	MaxFiles         int           `json:"max_files"`
	MaxFragments     int           `json:"max_fragments"`
	PreferSignatures bool          `json:"prefer_signatures"`
}

// handleChat compiles dynamic repository context and streams chat responses
// from OpenAI or Anthropic using Server-Sent Events (SSE).
func handleChat(w http.ResponseWriter, r *http.Request) {
	// 1. Decode body
	var req chatReq
	bodyBytes, ok := readLimitedBody(w, r)
	if !ok {
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	// 2. Validate repo
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}

	// 3. Open DB
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to open database: "+err.Error())
		return
	}
	defer closeProxyDB(db, "chat request")

	files, err := db.AllFiles()
	if err != nil || len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "index is empty — run scan first")
		return
	}

	// 4. Extract search query (from last user message)
	var query string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			query = req.Messages[i].Content
			break
		}
	}
	if strings.TrimSpace(query) == "" {
		writeErr(w, http.StatusBadRequest, "no user message found in history")
		return
	}

	// Get query embedding and all file embeddings
	embClient := embeddings.NewClient(cfg.HybridMode)
	queryEmb, _ := embClient.GetEmbedding(context.Background(), query)
	fileEmbs, _ := db.AllEmbeddings()
	rels, _ := db.AllRelations()

	// 5. Rank and package
	rankWeights, _, _ := ranking.LoadWeights(cfg.RepoRoot)
	rankOpts := ranking.Options{
		Project:        taskflow.LoadProjectInfo(db),
		ChangedFiles:   gitChangedFiles(cfg.RepoRoot),
		Focus:          req.Focus,
		QueryEmbedding: queryEmb,
		Embeddings:     fileEmbs,
		Relations:      rels,
		Weights:        &rankWeights,
	}
	ranked := ranking.RankWithOptions(files, query, rankOpts)

	budget := req.Budget
	if budget <= 0 {
		budget = config.DefaultBudget
	}

	bundle, err := packager.Pack(ranked, query, packager.Options{
		Budget:           budget,
		MaxFiles:         req.MaxFiles,
		MaxFragments:     req.MaxFragments,
		PreferSignatures: req.PreferSignatures,
		UpgradeWithSlack: true,
		QueryTerms:       ranking.Tokenise(query),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to pack context: "+err.Error())
		return
	}

	var promptBuf bytes.Buffer
	if err := output.WriteClaude(&promptBuf, bundle, taskflow.BuildRepoSummary(cfg.RepoRoot, files, taskflow.LoadProjectInfo(db))); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to build context XML: "+err.Error())
		return
	}
	contextText := promptBuf.String()

	// 6. Log proxy request statistics to SQLite and memory
	totalBytes := int64(0)
	for _, f := range files {
		totalBytes += f.Size
	}
	before := int(totalBytes / 4)
	after := bundle.Stats.TokensUsed
	saved := before - after
	if saved < 0 {
		saved = 0
	}
	rate := 3.00
	if req.Provider == "openai" {
		rate = 2.50
	}
	usd := float64(saved) * rate / 1000000.0
	_ = db.InsertProxyLog(time.Now(), req.Model, query, before, after, saved, usd)
	logProxyRequest(req.Model, query, before, after)

	// 7. Call upstream and stream
	if req.Provider == "openai" {
		apiKey := r.Header.Get("X-Openai-Api-Key")
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		if apiKey == "" {
			writeErr(w, http.StatusUnauthorized, "OPENAI_API_KEY is not set in environment or X-OpenAI-Api-Key header")
			return
		}

		// Construct OpenAI payload
		var messages []openAIMessage
		sysTextJSON, _ := json.Marshal(contextText)
		messages = append(messages, openAIMessage{
			Role:    "system",
			Content: sysTextJSON,
		})

		for _, msg := range req.Messages {
			contentJSON, _ := json.Marshal(msg.Content)
			messages = append(messages, openAIMessage{
				Role:    msg.Role,
				Content: contentJSON,
			})
		}

		upstreamReqBody := map[string]any{
			"model":    req.Model,
			"messages": messages,
			"stream":   true,
		}
		upstreamJSON, _ := json.Marshal(upstreamReqBody)
		openaiURL := "https://api.openai.com/v1/chat/completions"
		if testURL := os.Getenv("NEUROFS_TEST_OPENAI_URL"); testURL != "" {
			openaiURL = testURL
		}

		upstreamReq, err := http.NewRequest(http.MethodPost, openaiURL, bytes.NewReader(upstreamJSON))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to create upstream request: "+err.Error())
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(upstreamReq)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
			return
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Printf("neurofs proxy: close streaming upstream response: %v", err)
			}
		}()

		if resp.StatusCode != http.StatusOK {
			errBytes, truncated, readErr := readUpstreamErrorBody(resp.Body)
			if readErr != nil {
				writeErr(w, http.StatusBadGateway, "read upstream error response: "+readErr.Error())
				return
			}
			if truncated {
				w.Header().Set("X-NeuroFS-Upstream-Error-Truncated", "true")
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBytes)
			return
		}

		// Set streaming headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Write bundle metadata as an initial custom event so the UI can render the files list!
		metadataBytes, _ := json.Marshal(map[string]any{
			"tokens_before": before,
			"tokens_after":  after,
			"saved_tokens":  saved,
			"savings_usd":   usd,
			"fragments":     bundle.Fragments,
		})
		_, _ = fmt.Fprintf(w, "event: neurofs_metadata\ndata: %s\n\n", string(metadataBytes))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}

	} else {
		// Anthropic
		apiKey := r.Header.Get("X-Anthropic-Api-Key")
		if apiKey == "" {
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		if apiKey == "" {
			writeErr(w, http.StatusUnauthorized, "ANTHROPIC_API_KEY is not set in environment or X-Anthropic-Api-Key header")
			return
		}

		// Construct Anthropic messages and system prompt
		var messages []anthropicMessage
		for _, msg := range req.Messages {
			contentJSON, _ := json.Marshal(msg.Content)
			messages = append(messages, anthropicMessage{
				Role:    msg.Role,
				Content: contentJSON,
			})
		}

		systemBlocksJSON := buildSystemPrompt(nil, contextText)

		upstreamReqBody := map[string]any{
			"model":      req.Model,
			"system":     systemBlocksJSON,
			"messages":   messages,
			"max_tokens": 4096,
			"stream":     true,
		}
		upstreamJSON, _ := json.Marshal(upstreamReqBody)
		anthropicURL := "https://api.anthropic.com/v1/messages"
		if testURL := os.Getenv("NEUROFS_TEST_ANTHROPIC_URL"); testURL != "" {
			anthropicURL = testURL
		}

		upstreamReq, err := http.NewRequest(http.MethodPost, anthropicURL, bytes.NewReader(upstreamJSON))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to create upstream request: "+err.Error())
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("x-api-key", apiKey)
		upstreamReq.Header.Set("anthropic-version", "2023-06-01")

		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(upstreamReq)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
			return
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Printf("neurofs proxy: close streaming upstream response: %v", err)
			}
		}()

		if resp.StatusCode != http.StatusOK {
			errBytes, truncated, readErr := readUpstreamErrorBody(resp.Body)
			if readErr != nil {
				writeErr(w, http.StatusBadGateway, "read upstream error response: "+readErr.Error())
				return
			}
			if truncated {
				w.Header().Set("X-NeuroFS-Upstream-Error-Truncated", "true")
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBytes)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Write bundle metadata as an initial custom event so the UI can render the files list!
		metadataBytes, _ := json.Marshal(map[string]any{
			"tokens_before": before,
			"tokens_after":  after,
			"saved_tokens":  saved,
			"savings_usd":   usd,
			"fragments":     bundle.Fragments,
		})
		_, _ = fmt.Fprintf(w, "event: neurofs_metadata\ndata: %s\n\n", string(metadataBytes))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
	}
}

func readUpstreamErrorBody(body io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxUpstreamErrorBodyBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) <= maxUpstreamErrorBodyBytes {
		return data, false, nil
	}
	return data[:maxUpstreamErrorBodyBytes], true, nil
}
