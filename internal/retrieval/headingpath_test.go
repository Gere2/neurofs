package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

func TestHeadingPathBoost(t *testing.T) {
	w := defaultTestWeights()
	const headingPath = "Roadmap/Sprint S6/S6.1 Cancelación"

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		{"section_identifier", "S6.1", w.HeadingPathMatch},
		{"section_identifier_in_a_sentence", "que hace la seccion S6.1", w.HeadingPathMatch},
		{"parent_section_identifier", "que entra en el sprint S6", w.HeadingPathMatch},
		{"sibling_identifier_does_not_match", "S6.2", 0},
		{"unrelated_query", "packager budget manager", 0},
		// Plain heading words already score through symbol_match — boosting
		// them here too double-counts one signal and buries code under docs.
		{"ordinary_heading_word_is_not_boosted", "cancelación de pedidos", 0},
		{"ordinary_parent_word_is_not_boosted", "sprint roadmap", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := headingPathBoost(headingPath, c.query, w); got != c.want {
				t.Errorf("headingPathBoost(%q, %q) = %v, want %v", headingPath, c.query, got, c.want)
			}
		})
	}

	// The analysis that motivated this named SPRINTS.md#D-09 specifically.
	t.Run("dashed_section_identifier", func(t *testing.T) {
		if got := headingPathBoost("Sprints/D-09 Cierre de caja", "que falta en D-09", w); got != w.HeadingPathMatch {
			t.Errorf("D-09 must be matchable, got %v", got)
		}
		if got := headingPathBoost("Sprints/D-09 Cierre de caja", "que falta en D-10", w); got != 0 {
			t.Errorf("a different section must not match, got %v", got)
		}
	})
	t.Run("no_heading_path", func(t *testing.T) {
		if got := headingPathBoost("", "S6.1", w); got != 0 {
			t.Errorf("code chunks must never be boosted, got %v", got)
		}
	})
	t.Run("weight_zero_is_inert", func(t *testing.T) {
		zero := defaultTestWeights()
		zero.HeadingPathMatch = 0
		if got := headingPathBoost(headingPath, "S6.1", zero); got != 0 {
			t.Errorf("zero weight must be inert, got %v", got)
		}
	})
	t.Run("stop_words_are_not_distinctive", func(t *testing.T) {
		if got := headingPathBoost("The Roadmap/For Sprint", "what is the plan for", w); got != 0 {
			t.Errorf("stop words must not trigger a heading match, got %v", got)
		}
	})
	t.Run("identifiers_the_tokeniser_can_already_see_are_left_alone", func(t *testing.T) {
		// "2026" survives tokenisation, so the lexical path scores it.
		if got := headingPathBoost("Roadmap/Plan 2026", "plan 2026", w); got != 0 {
			t.Errorf("tokenisable words must not be double-counted, got %v", got)
		}
	})
}

// TestSearchRanksRequestedMarkdownSubsectionFirst indexes a real nested
// document and asserts the named subsection wins. Before heading paths,
// "S6.1" tokenised to nothing and every section of the file scored the same.
func TestSearchRanksRequestedMarkdownSubsectionFirst(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	doc := `# Roadmap

Plan general de entrega.

## Sprint S5

Trabajo de la iteracion anterior sobre inventario.

## Sprint S6

Iteracion en curso.

### S6.1 Cancelación

Reglas de cancelación de pedidos ya confirmados.

### S6.2 Devolución

Reglas de devolución parcial.

## Sprint S7

Iteracion siguiente sobre tesoreria.
`
	full := filepath.Join(repo, "SPRINTS.md")
	if err := os.WriteFile(full, []byte(doc), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.New(repo)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatalf("indexer.Run: %v", err)
	}
	chunks, err := db.GetChunksForFile(full)
	if err != nil {
		_ = db.Close()
		t.Fatalf("chunks: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}

	headingPaths := make(map[string]string)
	for _, chunk := range chunks {
		headingPaths[chunk.Symbol] = chunk.HeadingPath
	}
	if got, want := headingPaths["S6.1 Cancelación"], "Roadmap/Sprint S6/S6.1 Cancelación"; got != want {
		t.Fatalf("heading_path did not survive indexing: got %q, want %q", got, want)
	}
	if got, want := headingPaths["Sprint S5"], "Roadmap/Sprint S5"; got != want {
		t.Errorf("sibling heading_path = %q, want %q", got, want)
	}

	resp, err := Search(context.Background(), Options{
		Query: "S6.1",
		Repo:  repo,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected the named subsection to be retrievable")
	}
	if got := resp.Results[0].Symbol; got != "S6.1 Cancelación" {
		t.Fatalf("top result = %q, want the S6.1 section (results=%+v)", got, resp.Results)
	}
	if !containsString(resp.Results[0].Reasons, "heading_path_match") {
		t.Errorf("expected heading_path_match reason, got %v", resp.Results[0].Reasons)
	}
	for _, hit := range resp.Results[1:] {
		if hit.Score >= resp.Results[0].Score {
			t.Errorf("section %q (%v) must rank below S6.1 (%v)", hit.Symbol, hit.Score, resp.Results[0].Score)
		}
		if hit.Symbol == "Sprint S5" || hit.Symbol == "Sprint S7" {
			t.Errorf("unrequested sibling section %q outranked or tied S6.1", hit.Symbol)
		}
	}
}
