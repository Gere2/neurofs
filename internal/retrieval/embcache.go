package retrieval

import (
	"context"
	"sync"

	"github.com/neuromfs/neuromfs/internal/embeddings"
)

// Query embeddings are memoized per process. Two callers repeat queries
// heavily: an MCP session asking variations of the same question, and the
// learn tuner, which re-runs the same fixture queries hundreds of times
// with different weights — without this cache every candidate evaluation
// would re-hit the embedding provider. Failed lookups are not cached so a
// transient provider error can recover on the next call.
var (
	queryEmbMu    sync.Mutex
	queryEmbCache = map[string][]float32{}
)

const queryEmbCacheMax = 512

func cachedQueryEmbedding(ctx context.Context, hybridMode bool, query string) []float32 {
	client := embeddings.NewClient(hybridMode)
	key := client.ProviderName() + "|" + client.ModelName() + "|" + query

	queryEmbMu.Lock()
	if emb, ok := queryEmbCache[key]; ok {
		queryEmbMu.Unlock()
		return emb
	}
	queryEmbMu.Unlock()

	emb, err := client.GetEmbedding(ctx, query)
	if err != nil || len(emb) == 0 {
		return nil
	}

	queryEmbMu.Lock()
	if len(queryEmbCache) >= queryEmbCacheMax {
		queryEmbCache = map[string][]float32{}
	}
	queryEmbCache[key] = emb
	queryEmbMu.Unlock()
	return emb
}
