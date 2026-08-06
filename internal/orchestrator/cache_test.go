package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSemanticCache_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_cache.db")

	cache, err := NewSemanticCache(dbPath, 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to init cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	query := "Implement auth middleware"
	contextStr := "file auth.go line 1-50"
	model := "claude-sonnet"

	// Initial Get — should miss
	_, hit := cache.Get(ctx, query, contextStr, model, 0.80)
	if hit {
		t.Fatal("expected cache miss before put")
	}

	// Put entry
	entry := CacheEntry{
		Model:        model,
		Provider:     "anthropic",
		Prompt:       query,
		Response:     "package auth...",
		Grounding:    0.92,
		InputTokens:  120,
		OutputTokens: 80,
		CostUSD:      0.002,
	}

	err = cache.Put(ctx, query, contextStr, entry)
	if err != nil {
		t.Fatalf("cache put failed: %v", err)
	}

	// Get entry — should hit
	cached, hit := cache.Get(ctx, query, contextStr, model, 0.80)
	if !hit {
		t.Fatal("expected cache hit after put")
	}

	if cached.Response != entry.Response {
		t.Errorf("expected response %q, got %q", entry.Response, cached.Response)
	}
	if cached.Grounding != entry.Grounding {
		t.Errorf("expected grounding %.2f, got %.2f", entry.Grounding, cached.Grounding)
	}
	if cached.HitCount != 1 {
		t.Errorf("expected hit count 1, got %d", cached.HitCount)
	}

	// Second get — hit count should increment to 2
	cached2, hit := cache.Get(ctx, query, contextStr, model, 0.80)
	if !hit {
		t.Fatal("expected second cache hit")
	}
	if cached2.HitCount != 2 {
		t.Errorf("expected hit count 2, got %d", cached2.HitCount)
	}
}

func TestSemanticCache_MinGroundingFilter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_cache.db")

	cache, err := NewSemanticCache(dbPath, 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to init cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	query := "Write SQL migration"
	contextStr := "schema.sql"
	model := "gemini-flash"

	entry := CacheEntry{
		Model:     model,
		Provider:  "google",
		Prompt:    query,
		Response:  "CREATE TABLE...",
		Grounding: 0.70, // lower grounding
	}

	_ = cache.Put(ctx, query, contextStr, entry)

	// Get requiring min 0.85 grounding — should miss
	_, hit := cache.Get(ctx, query, contextStr, model, 0.85)
	if hit {
		t.Error("expected miss when minGrounding 0.85 > 0.70 entry grounding")
	}

	// Get requiring min 0.65 grounding — should hit
	_, hit = cache.Get(ctx, query, contextStr, model, 0.65)
	if !hit {
		t.Error("expected hit when minGrounding 0.65 <= 0.70 entry grounding")
	}
}

func TestSemanticCache_TTL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_cache.db")

	// 1 millisecond TTL
	cache, err := NewSemanticCache(dbPath, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("failed to init cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	query := "Fast query"
	contextStr := "ctx"
	model := "gpt-4o-mini"

	_ = cache.Put(ctx, query, contextStr, CacheEntry{
		Model:     model,
		Provider:  "openai",
		Prompt:    query,
		Response:  "resp",
		Grounding: 0.90,
	})

	time.Sleep(5 * time.Millisecond)

	// Should expire
	_, hit := cache.Get(ctx, query, contextStr, model, 0.80)
	if hit {
		t.Error("expected cache miss due to TTL expiration")
	}
}

func TestDispatcher_IntegratedCache(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dispatcher_cache.db")

	cache, err := NewSemanticCache(dbPath, 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to init cache: %v", err)
	}
	defer cache.Close()

	cfg := DefaultModelsConfig()
	router := NewRouter(cfg)
	mockClient := &cascadeMockClient{
		responses: map[string]string{
			"gemini-2.5-flash-preview-05-20": "initial response",
		},
	}

	dispatcher := NewDispatcher(router, mockClient)
	dispatcher.Cache = cache

	task := &Task{
		ID:          "t1",
		Description: "Build cache test task",
		Kind:        KindBackend,
		Complexity:  Simple,
		Context:     "test context",
	}

	groundFn := func(resp, ctx string) float64 { return 0.95 }

	// First execution — miss and store
	err = dispatcher.dispatchWithCascade(context.Background(), task, cfg.Cascade, groundFn, nil)
	if err != nil {
		t.Fatalf("first dispatch failed: %v", err)
	}
	if task.Cached {
		t.Error("first dispatch should not be cached")
	}

	// Reset task state
	task2 := &Task{
		ID:          "t1",
		Description: "Build cache test task",
		Kind:        KindBackend,
		Complexity:  Simple,
		Context:     "test context",
	}

	// Second execution — should HIT cache!
	err = dispatcher.dispatchWithCascade(context.Background(), task2, cfg.Cascade, groundFn, nil)
	if err != nil {
		t.Fatalf("second dispatch failed: %v", err)
	}

	if !task2.Cached {
		t.Error("second dispatch should hit semantic cache")
	}
	if task2.CostUSD != 0 {
		t.Errorf("expected 0 cost on cache hit, got %.4f", task2.CostUSD)
	}
}
