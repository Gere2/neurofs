package embeddings_test

import (
	"context"
	"math"
	"testing"

	"github.com/neuromfs/neuromfs/internal/embeddings"
)

func TestMockEmbeddingDeterminism(t *testing.T) {
	client := embeddings.NewClient()

	vec1, err := client.GetEmbedding(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("failed to get mock embedding: %v", err)
	}

	vec2, err := client.GetEmbedding(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("failed to get mock embedding: %v", err)
	}

	if len(vec1) != embeddings.Dimension {
		t.Errorf("expected dimension %d, got %d", embeddings.Dimension, len(vec1))
	}

	for i := 0; i < len(vec1); i++ {
		if vec1[i] != vec2[i] {
			t.Fatalf("expected deterministic mock vectors, but got different values at index %d: %f vs %f", i, vec1[i], vec2[i])
		}
	}
}

func TestMockEmbeddingUnitNormalization(t *testing.T) {
	client := embeddings.NewClient()

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
			sim := embeddings.CosineSimilarity(tc.v1, tc.v2)
			if math.Abs(sim-tc.expected) > 1e-6 {
				t.Errorf("expected similarity %f, got %f", tc.expected, sim)
			}
		})
	}
}

func TestEncodeDecodeEmbedding(t *testing.T) {
	vec := []float32{0.1, -0.5, 0.999, 123.456}

	encoded, err := embeddings.EncodeEmbedding(vec)
	if err != nil {
		t.Fatalf("failed to encode: %v", err)
	}

	decoded, err := embeddings.DecodeEmbedding(encoded)
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
