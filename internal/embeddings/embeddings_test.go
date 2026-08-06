package embeddings

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	// Case 1: Defaults to mock if no keys are set
	client := NewClient()
	if client.ProviderName() != "mock" {
		t.Errorf("expected provider mock, got %s", client.ProviderName())
	}

	// Case 2: Cloud keys alone do not opt repository contents into cloud use.
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	client = NewClient()
	if client.ProviderName() != "mock" {
		t.Errorf("expected provider mock without explicit opt-in, got %s", client.ProviderName())
	}

	// Case 3: The same rule applies to Gemini.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "ai-test-gemini")
	client = NewClient()
	if client.ProviderName() != "mock" {
		t.Errorf("expected provider mock without explicit opt-in, got %s", client.ProviderName())
	}

	// Case 4: The same rule applies to Voyage.
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "v-test-voyage")
	client = NewClient()
	if client.ProviderName() != "mock" {
		t.Errorf("expected provider mock without explicit opt-in, got %s", client.ProviderName())
	}

	// Case 5: Ollama responsive auto-detects ollama
	t.Setenv("VOYAGE_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("OLLAMA_HOST", server.URL)
	client = NewClient()
	if client.ProviderName() != "ollama" {
		t.Errorf("expected provider ollama, got %s", client.ProviderName())
	}
	if client.ModelName() != "nomic-embed-text" {
		t.Errorf("expected default Ollama model, got %s", client.ModelName())
	}

	// Case 6: Explicit setting overrides auto-detection
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test")
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
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode OpenAI response: %v", err)
		}
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
		if r.URL.RawQuery != "" {
			t.Errorf("Gemini credential leaked into query string: %q", r.URL.RawQuery)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("x-goog-api-key") != "ai-test-gemini" {
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
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode Gemini response: %v", err)
		}
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

func TestGeminiErrorsDoNotExposeEndpointOrAPIKey(t *testing.T) {
	const secret = "gemini-secret-that-must-not-leak"
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("Gemini credential leaked into query string: %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("x-goog-api-key"); got != secret {
			t.Errorf("x-goog-api-key = %q, want configured credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key ` + secret + ` at ` + serverURL + `"}}`))
	}))
	defer server.Close()
	serverURL = server.URL

	client := NewClient()
	client.provider = "gemini"
	client.apiKey = secret
	client.endpoint = server.URL
	client.model = "text-embedding-004"

	_, err := client.GetEmbedding(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Gemini error")
	}
	errText := err.Error()
	if strings.Contains(errText, secret) {
		t.Fatalf("error exposed API key: %q", errText)
	}
	if strings.Contains(errText, server.URL) {
		t.Fatalf("error exposed endpoint URL: %q", errText)
	}
}

func TestProviderErrorsDoNotExposeResponseBodies(t *testing.T) {
	const leaked = "repository-text-and-secret-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"` + leaked + `"}`))
	}))
	defer server.Close()

	for _, provider := range []string{"openai", "voyage", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			client := &Client{
				provider: provider,
				apiKey:   "configured-key",
				client:   server.Client(),
				model:    "test-model",
				endpoint: server.URL,
			}
			_, err := client.GetEmbedding(context.Background(), "private repository text")
			if err == nil {
				t.Fatal("expected provider error")
			}
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("provider response body leaked through error: %q", err)
			}
			if !strings.Contains(err.Error(), "status 502") {
				t.Fatalf("error omitted safe HTTP status: %q", err)
			}
		})
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
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode Voyage response: %v", err)
		}
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
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode Ollama response: %v", err)
		}
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
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("NEUROFS_HYBRID_MODE", "")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	// A cloud key alone is not an opt-in.
	clientNormal := NewClient()
	if clientNormal.ProviderName() != "mock" {
		t.Errorf("expected provider mock, got %s", clientNormal.ProviderName())
	}

	// If hybridMode param is true, it should bypass OpenAI and fall back to mock (since Ollama is not running in test)
	clientHybridParam := NewClient(true)
	if clientHybridParam.ProviderName() != "mock" {
		t.Errorf("expected provider mock in hybrid mode, got %s", clientHybridParam.ProviderName())
	}

	// If NEUROFS_HYBRID_MODE env is true, it should also bypass OpenAI and fall back to mock
	t.Setenv("NEUROFS_HYBRID_MODE", "true")
	clientHybridEnv := NewClient()
	if clientHybridEnv.ProviderName() != "mock" {
		t.Errorf("expected provider mock with hybrid mode env, got %s", clientHybridEnv.ProviderName())
	}
}

func TestCloudEmbeddingFailurePreservesProviderProvenance(t *testing.T) {
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

	_, err := client.GetEmbedding(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected the configured cloud provider failure to be returned")
	}

	// A request failure must never mutate provenance or silently mix vector
	// spaces in one indexing run.
	if client.ProviderName() != "openai" {
		t.Errorf("expected provider to remain openai, got %s", client.ProviderName())
	}
	if client.ModelName() != "text-embedding-3-small" {
		t.Errorf("expected model provenance to remain stable, got %s", client.ModelName())
	}
}

func TestHybridModeOverridesExplicitCloudProvider(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	client := NewClient(true)
	if client.ProviderName() != "mock" {
		t.Fatalf("hybrid mode provider = %q, want local mock", client.ProviderName())
	}
}

func TestInvalidExplicitProviderIsNotSilentlyMocked(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "opneai")
	client := NewClient()
	if client.ProviderName() != "opneai" {
		t.Fatalf("provider = %q, want explicit invalid value preserved", client.ProviderName())
	}
	if _, err := client.GetEmbedding(context.Background(), "hello"); err == nil {
		t.Fatal("expected unsupported provider error")
	}
	if err := client.Validate(); err == nil {
		t.Fatal("expected unsupported provider validation error")
	}
}

func TestValidateRejectsMissingExplicitCloudCredential(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "gemini")
	t.Setenv("GEMINI_API_KEY", "")
	client := NewClient()
	if err := client.Validate(); err == nil {
		t.Fatal("expected missing Gemini API key validation error")
	}
}
