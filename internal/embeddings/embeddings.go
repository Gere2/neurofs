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
	"time"
)

// Dimension is the dimension of OpenAI's text-embedding-3-small embeddings.
const Dimension = 1536

// Client generates embeddings using OpenAI API or falls back to a deterministic mock
// when no API key is available.
type Client struct {
	apiKey string
	client *http.Client
}

// NewClient returns a new Client. It reads OPENAI_API_KEY from the environment.
func NewClient() *Client {
	return &Client{
		apiKey: os.Getenv("OPENAI_API_KEY"),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// HasAPIKey reports whether the client has an active API key.
func (c *Client) HasAPIKey() bool {
	return c.apiKey != ""
}

// GetEmbedding returns the 1536-dimensional embedding vector for the text.
// If no API key is set, it falls back to a deterministic mock vector.
func (c *Client) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	if c.apiKey == "" {
		return c.getMockEmbedding(text), nil
	}

	reqBody := map[string]any{
		"model": "text-embedding-3-small",
		"input": text,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(bodyBytes))
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
