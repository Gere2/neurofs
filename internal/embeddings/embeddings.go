package embeddings

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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

// NewClient returns a new Client based on environment variables, config, and auto-detection.
func NewClient(hybridMode ...bool) *Client {
	forceLocal := false
	if len(hybridMode) > 0 && hybridMode[0] {
		forceLocal = true
	}
	if os.Getenv("NEUROFS_HYBRID_MODE") == "true" {
		forceLocal = true
	}

	provider := os.Getenv("NEUROFS_EMBEDDING_PROVIDER")
	apiKeyOpenAI := os.Getenv("OPENAI_API_KEY")
	apiKeyGemini := os.Getenv("GEMINI_API_KEY")
	apiKeyVoyage := os.Getenv("VOYAGE_API_KEY")
	ollamaHost := os.Getenv("OLLAMA_HOST")
	if ollamaHost == "" {
		ollamaHost = "http://localhost:11434"
	}

	netClient := &http.Client{Timeout: 15 * time.Second}

	// Auto-detect provider if not explicitly configured
	if provider == "" {
		if !forceLocal && apiKeyOpenAI != "" {
			provider = "openai"
		} else if !forceLocal && apiKeyGemini != "" {
			provider = "gemini"
		} else if !forceLocal && apiKeyVoyage != "" {
			provider = "voyage"
		} else if isOllamaAvailable(netClient, ollamaHost) {
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
	default:
		provider = "mock"
		model = "mock-lcg"
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
	default:
		return c.getMockEmbedding(text), nil
	}

	if err != nil && (c.provider == "openai" || c.provider == "gemini" || c.provider == "voyage") {
		fmt.Fprintf(os.Stderr, "embeddings: cloud provider %s failed (%v), falling back to local embeddings\n", c.provider, err)
		ollamaHost := os.Getenv("OLLAMA_HOST")
		if ollamaHost == "" {
			ollamaHost = "http://localhost:11434"
		}
		if isOllamaAvailable(c.client, ollamaHost) {
			c.provider = "ollama"
			c.endpoint = ollamaHost
			c.model = os.Getenv("OLLAMA_MODEL")
			if c.model == "" {
				c.model = "nomic-embed-text"
			}
			return c.getOllamaEmbedding(ctx, text)
		} else {
			c.provider = "mock"
			c.model = "mock-lcg"
			return c.getMockEmbedding(text), nil
		}
	}

	return emb, err
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		return nil, fmt.Errorf("openai error (status %d): %s", resp.StatusCode, errData.Error.Message)
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
	url := fmt.Sprintf("%s/v1beta/%s:embedContent?key=%s", strings.TrimSuffix(host, "/"), modelName, c.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		return nil, fmt.Errorf("gemini error (status %d): %s", resp.StatusCode, errData.Error.Message)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		if errData.Detail != "" {
			return nil, fmt.Errorf("voyage error (status %d): %s", resp.StatusCode, errData.Detail)
		}
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("voyage error (status %d): %s", resp.StatusCode, string(body))
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama error (status %d): %s", resp.StatusCode, string(body))
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
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
