package embeddings

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Dimension is the dimension of OpenAI's text-embedding-3-small and mock embeddings.
const Dimension = 1536

// Client generates embeddings using OpenAI API, Gemini API, Voyage AI API,
// Ollama API, or falls back to a deterministic mock.
type Client struct {
	provider string
	apiKey   string
	client   *http.Client
	model    string
	endpoint string
}

// NewClient returns a new Client based on environment variables and config.
//
// Cloud providers are opt-in: merely having a provider API key in the
// environment must not make an indexing command send repository contents over
// the network. Set NEUROFS_EMBEDDING_PROVIDER explicitly to openai, gemini, or
// voyage to enable a cloud provider. Without an explicit provider, NeuroFS
// prefers a reachable local Ollama instance and otherwise uses the deterministic
// mock provider.
func NewClient(hybridMode ...bool) *Client {
	forceLocal := len(hybridMode) > 0 && hybridMode[0]
	if os.Getenv("NEUROFS_HYBRID_MODE") == "true" {
		forceLocal = true
	}

	provider := strings.ToLower(strings.TrimSpace(os.Getenv("NEUROFS_EMBEDDING_PROVIDER")))
	apiKeyOpenAI := os.Getenv("OPENAI_API_KEY")
	apiKeyGemini := os.Getenv("GEMINI_API_KEY")
	apiKeyVoyage := os.Getenv("VOYAGE_API_KEY")
	ollamaHost := os.Getenv("OLLAMA_HOST")
	if ollamaHost == "" {
		ollamaHost = "http://localhost:11434"
	}

	netClient := &http.Client{Timeout: 15 * time.Second}

	// Hybrid mode is a local-only boundary even when a cloud provider was
	// accidentally left configured in the environment.
	if forceLocal && isCloudProvider(provider) {
		provider = ""
	}

	// Auto-detection is deliberately local-only. Cloud use requires the
	// explicit provider setting above.
	if provider == "" {
		if isOllamaAvailable(netClient, ollamaHost) {
			provider = "ollama"
		} else {
			provider = "mock"
		}
	}

	var apiKey string
	var model string
	switch provider {
	case "openai":
		apiKey = apiKeyOpenAI
		model = os.Getenv("OPENAI_EMBEDDING_MODEL")
		if model == "" {
			model = "text-embedding-3-small"
		}
	case "gemini":
		apiKey = apiKeyGemini
		model = os.Getenv("GEMINI_EMBEDDING_MODEL")
		if model == "" {
			model = "text-embedding-004"
		}
	case "voyage":
		apiKey = apiKeyVoyage
		model = os.Getenv("VOYAGE_EMBEDDING_MODEL")
		if model == "" {
			model = "voyage-code-2"
		}
	case "ollama":
		model = os.Getenv("OLLAMA_MODEL")
		if model == "" {
			model = "nomic-embed-text"
		}
	case "mock":
		provider = "mock"
		model = "mock-lcg"
	default:
		// Keep an invalid explicit provider visible. Silently converting a typo
		// to mock would make the persisted provider metadata disagree with user
		// intent and hide a configuration error.
		model = "invalid"
	}

	endpoint := ""
	if provider == "ollama" {
		endpoint = ollamaHost
	}

	return &Client{
		provider: provider,
		apiKey:   apiKey,
		client:   netClient,
		model:    model,
		endpoint: endpoint,
	}
}

// ProviderName returns the active embedding provider name.
func (c *Client) ProviderName() string {
	return c.provider
}

// ModelName returns the active embedding model name.
func (c *Client) ModelName() string {
	return c.model
}

// HasAPIKey reports whether the client has an active API key (or doesn't need one).
func (c *Client) HasAPIKey() bool {
	if c.provider == "mock" || c.provider == "ollama" {
		return true
	}
	return c.apiKey != ""
}

// Validate checks provider configuration without sending any content or
// making a network request. Indexing calls this before clearing or mutating a
// previous index so a typo or missing cloud credential cannot destroy a valid
// generation and then enter an endless partial-reindex loop.
func (c *Client) Validate() error {
	switch c.provider {
	case "mock":
		if c.model == "" {
			return fmt.Errorf("mock embedding model is empty")
		}
	case "ollama":
		if c.model == "" {
			return fmt.Errorf("ollama embedding model is empty")
		}
		if strings.TrimSpace(c.endpoint) == "" {
			return fmt.Errorf("ollama endpoint is empty")
		}
	case "openai", "gemini", "voyage":
		if c.model == "" {
			return fmt.Errorf("%s embedding model is empty", c.provider)
		}
		if strings.TrimSpace(c.apiKey) == "" {
			return fmt.Errorf("%s embedding API key is not set", c.provider)
		}
	default:
		return fmt.Errorf("unsupported embedding provider %q", c.provider)
	}
	return nil
}

// GetEmbedding returns the embedding vector for the text based on the active provider.
func (c *Client) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	var emb []float32
	var err error

	switch c.provider {
	case "openai":
		emb, err = c.getOpenAIEmbedding(ctx, text)
	case "gemini":
		emb, err = c.getGeminiEmbedding(ctx, text)
	case "voyage":
		emb, err = c.getVoyageEmbedding(ctx, text)
	case "ollama":
		emb, err = c.getOllamaEmbedding(ctx, text)
	case "mock":
		return c.getMockEmbedding(text), nil
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", c.provider)
	}

	return emb, err
}

func isCloudProvider(provider string) bool {
	switch provider {
	case "openai", "gemini", "voyage":
		return true
	default:
		return false
	}
}

func (c *Client) getOpenAIEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("openai embedding: API key not set")
	}

	reqBody := map[string]any{
		"model": c.model,
		"input": text,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	host := c.endpoint
	if host == "" {
		host = "https://api.openai.com"
	}
	url := strings.TrimSuffix(host, "/") + "/v1/embeddings"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Provider bodies are untrusted and may echo repository text or
		// credential material. The status is sufficient for diagnosis.
		return nil, fmt.Errorf("openai error (status %d)", resp.StatusCode)
	}

	var respData struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(respData.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return respData.Data[0].Embedding, nil
}

func (c *Client) getGeminiEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("gemini embedding: API key not set")
	}

	modelName := c.model
	if !strings.HasPrefix(modelName, "models/") {
		modelName = "models/" + modelName
	}

	reqBody := map[string]any{
		"model": modelName,
		"content": map[string]any{
			"parts": []map[string]any{
				{"text": text},
			},
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	host := c.endpoint
	if host == "" {
		host = "https://generativelanguage.googleapis.com"
	}
	endpoint := fmt.Sprintf("%s/v1beta/%s:embedContent", strings.TrimSuffix(host, "/"), modelName)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("gemini embedding: create request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("gemini request canceled: %w", ctxErr)
		}
		// net/http transport errors often include the request URL. Keep Gemini
		// endpoint and credential material out of user-facing/logged errors.
		return nil, fmt.Errorf("gemini request failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Provider error bodies are untrusted and may echo request details.
		return nil, fmt.Errorf("gemini error (status %d)", resp.StatusCode)
	}

	var respData struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(respData.Embedding.Values) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return respData.Embedding.Values, nil
}

func (c *Client) getVoyageEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("voyage embedding: API key not set")
	}

	reqBody := map[string]any{
		"model": c.model,
		"input": []string{text},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	host := c.endpoint
	if host == "" {
		host = "https://api.voyageai.com"
	}
	url := strings.TrimSuffix(host, "/") + "/v1/embeddings"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voyage error (status %d)", resp.StatusCode)
	}

	var respData struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(respData.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return respData.Data[0].Embedding, nil
}

func (c *Client) getOllamaEmbedding(ctx context.Context, text string) ([]float32, error) {
	reqBody := map[string]any{
		"model":  c.model,
		"prompt": text,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(c.endpoint, "/") + "/api/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama error (status %d)", resp.StatusCode)
	}

	var respData struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(respData.Embedding) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return respData.Embedding, nil
}

// getMockEmbedding returns a unit-normalized deterministic vector derived from the input text
// so cosine similarity is deterministic and testable offline.
func (c *Client) getMockEmbedding(text string) []float32 {
	vec := make([]float32, Dimension)
	// Seed with hash of the text
	var h uint32 = 5381
	for _, r := range text {
		h = ((h << 5) + h) + uint32(r)
	}

	var sumSq float64
	for i := 0; i < Dimension; i++ {
		// Linear congruential generator for mock values
		h = h*1103515245 + 12345
		val := float64(h) / float64(math.MaxUint32) // [0, 1]
		vec[i] = float32(val - 0.5)                 // [-0.5, 0.5]
		sumSq += float64(vec[i] * vec[i])
	}

	// Normalize to unit vector
	norm := math.Sqrt(sumSq)
	if norm > 0 {
		for i := 0; i < Dimension; i++ {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}

	return vec
}

// CosineSimilarity calculates the cosine similarity between two unit vectors.
func CosineSimilarity(v1, v2 []float32) float64 {
	if len(v1) != len(v2) || len(v1) == 0 {
		return 0
	}
	var dot, n1, n2 float64
	for i := 0; i < len(v1); i++ {
		x := float64(v1[i])
		y := float64(v2[i])
		dot += x * y
		n1 += x * x
		n2 += y * y
	}
	if n1 == 0 || n2 == 0 {
		return 0
	}
	return dot / (math.Sqrt(n1) * math.Sqrt(n2))
}

// EncodeEmbedding serializes a float32 slice to compact binary representation.
func EncodeEmbedding(vec []float32) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, vec)
	return buf.Bytes(), err
}

// DecodeEmbedding deserializes a float32 slice from its binary format.
func DecodeEmbedding(data []byte) ([]float32, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("invalid binary length: %d", len(data))
	}
	vec := make([]float32, len(data)/4)
	err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &vec)
	return vec, err
}

func isOllamaAvailable(client *http.Client, host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	url := strings.TrimSuffix(host, "/") + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
