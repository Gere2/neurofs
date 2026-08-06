package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// CacheEntry is one cached response from an LLM execution.
type CacheEntry struct {
	Key          string    `json:"key"`
	QueryHash    string    `json:"query_hash"`
	ContextHash  string    `json:"context_hash"`
	Model        string    `json:"model"`
	Provider     string    `json:"provider"`
	Prompt       string    `json:"prompt"`
	Response     string    `json:"response"`
	Grounding    float64   `json:"grounding"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	HitCount     int       `json:"hit_count"`
	CreatedAt    time.Time `json:"created_at"`
	LastHitAt    time.Time `json:"last_hit_at"`
}

// SemanticCache provides SQLite-backed response caching for model completions.
type SemanticCache struct {
	mu     sync.Mutex
	db     *sql.DB
	dbPath string
	ttl    time.Duration
}

// NewSemanticCache initializes or connects to the SQLite response cache at dbPath.
// If dbPath is empty, defaults to ~/.neurofs/response_cache.db.
func NewSemanticCache(dbPath string, ttl time.Duration) (*SemanticCache, error) {
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("user home dir: %w", err)
		}
		dbPath = filepath.Join(home, ".neurofs", "response_cache.db")
	}

	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite cache: %w", err)
	}

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %s: %w", p, err)
		}
	}

	createTableSQL := `
	CREATE TABLE IF NOT EXISTS response_cache (
		key TEXT PRIMARY KEY,
		query_hash TEXT NOT NULL,
		context_hash TEXT NOT NULL,
		model TEXT NOT NULL,
		provider TEXT NOT NULL,
		prompt TEXT NOT NULL,
		response TEXT NOT NULL,
		grounding REAL NOT NULL,
		input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		cost_usd REAL NOT NULL,
		hit_count INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_hit_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_query_ctx ON response_cache(query_hash, context_hash, model);
	`
	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create cache schema: %w", err)
	}

	return &SemanticCache{
		db:     db,
		dbPath: dbPath,
		ttl:    ttl,
	}, nil
}

// ComputeCacheKey returns a SHA256 key for a (query, context, model) combination.
func ComputeCacheKey(query, contextStr, model string) (key, queryHash, contextHash string) {
	qHashBytes := sha256.Sum256([]byte(query))
	cHashBytes := sha256.Sum256([]byte(contextStr))
	queryHash = hex.EncodeToString(qHashBytes[:16])
	contextHash = hex.EncodeToString(cHashBytes[:16])

	fullBytes := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", queryHash, contextHash, model)))
	key = hex.EncodeToString(fullBytes[:])
	return key, queryHash, contextHash
}

// Get checks if a valid, unexpired cached completion exists.
func (c *SemanticCache) Get(ctx context.Context, query, contextStr, model string, minGrounding float64) (*CacheEntry, bool) {
	if c == nil || c.db == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key, _, _ := ComputeCacheKey(query, contextStr, model)

	querySQL := `
	SELECT key, query_hash, context_hash, model, provider, prompt, response,
	       grounding, input_tokens, output_tokens, cost_usd, hit_count, created_at, last_hit_at
	FROM response_cache
	WHERE key = ? AND grounding >= ?
	`

	var entry CacheEntry
	var createdAtUnix, lastHitAtUnix int64

	err := c.db.QueryRowContext(ctx, querySQL, key, minGrounding).Scan(
		&entry.Key, &entry.QueryHash, &entry.ContextHash, &entry.Model, &entry.Provider,
		&entry.Prompt, &entry.Response, &entry.Grounding, &entry.InputTokens, &entry.OutputTokens,
		&entry.CostUSD, &entry.HitCount, &createdAtUnix, &lastHitAtUnix,
	)
	if err != nil {
		return nil, false
	}

	entry.CreatedAt = time.Unix(createdAtUnix, 0)
	entry.LastHitAt = time.Unix(lastHitAtUnix, 0)

	// Check TTL
	if time.Since(entry.CreatedAt) > c.ttl {
		_, _ = c.db.ExecContext(ctx, "DELETE FROM response_cache WHERE key = ?", key)
		return nil, false
	}

	now := time.Now().Unix()
	entry.HitCount++
	entry.LastHitAt = time.Unix(now, 0)
	_, _ = c.db.ExecContext(ctx, "UPDATE response_cache SET hit_count = hit_count + 1, last_hit_at = ? WHERE key = ?", now, key)

	return &entry, true
}

// Put stores a new successful, grounded completion entry in the cache.
func (c *SemanticCache) Put(ctx context.Context, query, contextStr string, entry CacheEntry) error {
	if c == nil || c.db == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key, qHash, cHash := ComputeCacheKey(query, contextStr, entry.Model)
	entry.Key = key
	entry.QueryHash = qHash
	entry.ContextHash = cHash

	now := time.Now().Unix()

	insertSQL := `
	INSERT OR REPLACE INTO response_cache (
		key, query_hash, context_hash, model, provider, prompt, response,
		grounding, input_tokens, output_tokens, cost_usd, hit_count, created_at, last_hit_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`

	_, err := c.db.ExecContext(ctx, insertSQL,
		entry.Key, entry.QueryHash, entry.ContextHash, entry.Model, entry.Provider,
		entry.Prompt, entry.Response, entry.Grounding, entry.InputTokens, entry.OutputTokens,
		entry.CostUSD, entry.HitCount, now, now,
	)
	return err
}

// Close closes the SQLite connection.
func (c *SemanticCache) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.db.Close()
}
