package cli

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/retrieval"
)

func TestChunkHitsFromSearchMapsLanguageAndRange(t *testing.T) {
	searchRes := retrieval.Response{Results: []retrieval.Hit{{
		Path:          "src/auth.go",
		StartLine:     7,
		EndLine:       11,
		Kind:          "func",
		Symbol:        "VerifyJWT",
		Score:         9.5,
		Reasons:       []string{"exact_content"},
		TokenEstimate: 32,
		ContentHash:   "abc",
		Snippet:       "func VerifyJWT() bool { return true }",
	}}}
	files := []models.FileRecord{{
		RelPath: "src/auth.go",
		Lang:    models.LangGo,
	}}

	hits := chunkHitsFromSearch(searchRes, files)
	if len(hits) != 1 {
		t.Fatalf("expected one hit, got %d", len(hits))
	}
	hit := hits[0]
	if hit.Lang != models.LangGo || hit.StartLine != 7 || hit.EndLine != 11 {
		t.Fatalf("unexpected mapped hit: %+v", hit)
	}
	if hit.Symbol != "VerifyJWT" || hit.Reasons[0] != "exact_content" {
		t.Fatalf("unexpected symbol/reasons: %+v", hit)
	}
}
