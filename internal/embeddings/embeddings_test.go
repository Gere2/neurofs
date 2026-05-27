package embeddings

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func TestMockEmbeddingDeterminism(t *testing.T) {
	client := NewClient()

	vec1, err := client.GetEmbedding(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("failed to get mock embedding: %v", err)
	}

	vec2, err := client.GetEmbedding(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("failed to get mock embedding: %v", err)
	}

	if len(vec1) != Dimension {
		t.Errorf("expected dimension %d, got %d", Dimension, len(vec1))
	}

	for i := 0; i < len(vec1); i++ {
		if vec1[i] != vec2[i] {
			t.Fatalf("expected deterministic mock vectors, but got different values at index %d: %f vs %f", i, vec1[i], vec2[i])
		}
	}
}

func TestMockEmbeddingUnitNormalization(t *testing.T) {
	client := NewClient()

	vec, err := client.GetEmbedding(context.Background(), "test text")
	if err != nil {
		t.Fatalf("failed to get mock embedding: %v", err)
	}

	var sumSq float64
	for _, val := range vec {
		sumSq += float64(val * val)
	}

	// Sum of squares of a unit vector should be 1.0
	if math.Abs(sumSq-1.0) > 1e-5 {
		t.Errorf("expected unit vector norm to be 1.0, got %f", math.Sqrt(sumSq))
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		v1       []float32
		v2       []float32
		expected float64
	}{
		{
			name:     "identical vectors",
			v1:       []float32{1.0, 0.0, 0.0},
			v2:       []float32{1.0, 0.0, 0.0},
			expected: 1.0,
		},
		{
			name:     "orthogonal vectors",
			v1:       []float32{1.0, 0.0, 0.0},
			v2:       []float32{0.0, 1.0, 0.0},
			expected: 0.0,
		},
		{
			name:     "opposite vectors",
			v1:       []float32{1.0, 0.0},
			v2:       []float32{-1.0, 0.0},
			expected: -1.0,
		},
		{
			name:     "different lengths",
			v1:       []float32{1.0, 0.0},
			v2:       []float32{1.0, 0.0, 0.0},
			expected: 0.0,
		},
		{
			name:     "empty vectors",
			v1:       []float32{},
			v2:       []float32{},
			expected: 0.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sim := CosineSimilarity(tc.v1, tc.v2)
			if math.Abs(sim-tc.expected) > 1e-6 {
				t.Errorf("expected similarity %f, got %f", tc.expected, sim)
			}
		})
	}
}

func TestEncodeDecodeEmbedding(t *testing.T) {
	vec := []float32{0.1, -0.5, 0.999, 123.456}

	encoded, err := EncodeEmbedding(vec)
	if err != nil {
		t.Fatalf("failed to encode: %v", err)
	}

	decoded, err := DecodeEmbedding(encoded)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(vec) != len(decoded) {
		t.Fatalf("expected length %d, got %d", len(vec), len(decoded))
	}

	for i := 0; i < len(vec); i++ {
		if vec[i] != decoded[i] {
			t.Errorf("mismatch at index %d: expected %f, got %f", i, vec[i], decoded[i])
		}
	}
}

func TestAutoDetection(t *testing.T) {
	origProvider := os.Getenv("NEUROFS_EMBEDDING_PROVIDER")
	origOpenAIKey := os.Getenv("OPENAI_API_KEY")
	origGeminiKey := os.Getenv("GEMINI_API_KEY")
	origVoyageKey := os.Getenv("VOYAGE_API_KEY")
	origOllamaHost := os.Getenv("OLLAMA_HOST")
	defer func() {
		os.Setenv("NEUROFS_EMBEDDING_PROVIDER", origProvider)
		os.Setenv("OPENAI_API_KEY", origOpenAIKey)
		os.Setenv("GEMINI_API_KEY", origGeminiKey)
		os.Setenv("VOYAGE_API_KEY", origVoyageKey)
		os.Setenv("OLLAMA_HOST", origOllamaHost)
	}()

	os.Unsetenv("NEUROFS_EMBEDDING_PROVIDER")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("GEMINI_API_KEY")
	os.Unsetenv("VOYAGE_API_KEY")
	os.Unsetenv("OLLAMA_HOST")

	// Case 1: Defaults to mock if no keys are set
	client := NewClient()
	if client.ProviderName() != "mock" {
		t.Errorf("expected provider mock, got %s", client.ProviderName())
	}

	// Case 2: OpenAI Key auto-detects openai
	os.Setenv("OPENAI_API_KEY", "sk-test-openai")
	client = NewClient()
	if client.ProviderName() != "openai" {
		t.Errorf("expected provider openai, got %s", client.ProviderName())
	}
	if client.ModelName() != "text-embedding-3-small" {
		t.Errorf("expected default OpenAI model, got %s", client.ModelName())
	}

	// Case 3: Gemini Key auto-detects gemini
	os.Unsetenv("OPENAI_API_KEY")
	os.Setenv("GEMINI_API_KEY", "ai-test-gemini")
	client = NewClient()
	if client.ProviderName() != "gemini" {
		t.Errorf("expected provider gemini, got %s", client.ProviderName())
	}
	if client.ModelName() != "text-embedding-004" {
		t.Errorf("expected default Gemini model, got %s", client.ModelName())
	}

	// Case 4: Voyage Key auto-detects voyage
	os.Unsetenv("GEMINI_API_KEY")
	os.Setenv("VOYAGE_API_KEY", "v-test-voyage")
	client = NewClient()
	if client.ProviderName() != "voyage" {
		t.Errorf("expected provider voyage, got %s", client.ProviderName())
	}
	if client.ModelName() != "voyage-code-2" {
		t.Errorf("expected default Voyage model, got %s", client.ModelName())
	}

	// Case 5: Ollama responsive auto-detects ollama
	os.Unsetenv("VOYAGE_API_KEY")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	os.Setenv("OLLAMA_HOST", server.URL)
	client = NewClient()
	if client.ProviderName() != "ollama" {
		t.Errorf("expected provider ollama, got %s", client.ProviderName())
	}
	if client.ModelName() != "nomic-embed-text" {
		t.Errorf("expected default Ollama model, got %s", client.ModelName())
	}

	// Case 6: Explicit setting overrides auto-detection
	os.Setenv("NEUROFS_EMBEDDING_PROVIDER", "openai")
	os.Setenv("OPENAI_API_KEY", "sk-test")
	client = NewClient()
	if client.ProviderName() != "openai" {
		t.Errorf("expected explicit provider openai, got %s", client.ProviderName())
	}
}

func TestOpenAIEmbeddingAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test-openai" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["input"] != "hello" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"data": []map[string]any{
				{
					"embedding": []float32{0.1, 0.2, 0.3},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient()
	client.provider = "openai"
	client.apiKey = "sk-test-openai"
	client.endpoint = server.URL
	client.model = "text-embedding-3-small"

	vec, err := client.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []float32{0.1, 0.2, 0.3}
	if !reflect.DeepEqual(vec, expected) {
		t.Errorf("expected %v, got %v", expected, vec)
	}
}

func TestGeminiEmbeddingAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/text-embedding-004:embedContent" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("key") != "ai-test-gemini" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"embedding": map[string]any{
				"values": []float32{0.4, 0.5, 0.6},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient()
	client.provider = "gemini"
	client.apiKey = "ai-test-gemini"
	client.endpoint = server.URL
	client.model = "text-embedding-004"

	vec, err := client.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []float32{0.4, 0.5, 0.6}
	if !reflect.DeepEqual(vec, expected) {
		t.Errorf("expected %v, got %v", expected, vec)
	}
}

func TestVoyageEmbeddingAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer v-test-voyage" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inputs, ok := body["input"].([]any)
		if !ok || len(inputs) == 0 || inputs[0] != "hello" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"embedding": []float32{0.05, 0.15, 0.25},
					"index":     0,
				},
			},
			"model": "voyage-code-2",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient()
	client.provider = "voyage"
	client.apiKey = "v-test-voyage"
	client.endpoint = server.URL
	client.model = "voyage-code-2"

	vec, err := client.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []float32{0.05, 0.15, 0.25}
	if !reflect.DeepEqual(vec, expected) {
		t.Errorf("expected %v, got %v", expected, vec)
	}
}

func TestOllamaEmbeddingAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["model"] != "nomic-embed-text" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"embedding": []float32{0.7, 0.8, 0.9},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient()
	client.provider = "ollama"
	client.endpoint = server.URL
	client.model = "nomic-embed-text"

	vec, err := client.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []float32{0.7, 0.8, 0.9}
	if !reflect.DeepEqual(vec, expected) {
		t.Errorf("expected %v, got %v", expected, vec)
	}
}

func TestNewClientHybridMode(t *testing.T) {
	origOpenAIKey := os.Getenv("OPENAI_API_KEY")
	origHybridEnv := os.Getenv("NEUROFS_HYBRID_MODE")
	defer func() {
		os.Setenv("OPENAI_API_KEY", origOpenAIKey)
		os.Setenv("NEUROFS_HYBRID_MODE", origHybridEnv)
	}()

	os.Setenv("OPENAI_API_KEY", "sk-test-openai")

	// If hybridMode param is not passed and env is not set, it should pick OpenAI (since API key is set)
	clientNormal := NewClient()
	if clientNormal.ProviderName() != "openai" {
		t.Errorf("expected provider openai, got %s", clientNormal.ProviderName())
	}

	// If hybridMode param is true, it should bypass OpenAI and fall back to mock (since Ollama is not running in test)
	clientHybridParam := NewClient(true)
	if clientHybridParam.ProviderName() != "mock" {
		t.Errorf("expected provider mock in hybrid mode, got %s", clientHybridParam.ProviderName())
	}

	// If NEUROFS_HYBRID_MODE env is true, it should also bypass OpenAI and fall back to mock
	os.Setenv("NEUROFS_HYBRID_MODE", "true")
	clientHybridEnv := NewClient()
	if clientHybridEnv.ProviderName() != "mock" {
		t.Errorf("expected provider mock with hybrid mode env, got %s", clientHybridEnv.ProviderName())
	}
}

func TestCloudEmbeddingFallback(t *testing.T) {
	// Start a mock server that returns 500 error for OpenAI embeddings
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient()
	client.provider = "openai"
	client.apiKey = "sk-test-openai"
	client.endpoint = server.URL
	client.model = "text-embedding-3-small"

	// When GetEmbedding fails on OpenAI, it should fall back to Mock (since Ollama is not running in test)
	vec, err := client.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error, should have fallen back successfully: %v", err)
	}

	// Client provider should have switched to mock
	if client.ProviderName() != "mock" {
		t.Errorf("expected provider to fall back to mock, got %s", client.ProviderName())
	}

	// Verify we got a non-empty mock vector
	if len(vec) != Dimension {
		t.Errorf("expected mock dimension %d, got %d", Dimension, len(vec))
	}
}
