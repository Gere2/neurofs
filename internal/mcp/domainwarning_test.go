package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

func TestDomainWarningForVisualAssetQuery(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		topPath string
		want    bool
	}{
		{
			name:    "spanish_brand_asset_query_answered_with_code",
			query:   "Localizar activos visuales del cartel de marca",
			topPath: "apps/brain/lib/documents/identity.ts",
			want:    true,
		},
		{
			name:    "english_logo_png_query_answered_with_code",
			query:   "where is the logo png exported",
			topPath: "apps/pos/src/lib/product-service.ts",
			want:    true,
		},
		{
			name:    "single_visual_term_is_not_enough",
			query:   "how does the poster service build its payload",
			topPath: "apps/brain/lib/documents/identity.ts",
			want:    false,
		},
		{
			name:    "top_hit_already_inside_an_asset_directory",
			query:   "cartel de marca visual",
			topPath: "output/creative/poster.ts",
			want:    false,
		},
		{
			name:    "top_hit_is_documentation_not_code",
			query:   "cartel de marca visual",
			topPath: "docs/brand-guidelines.md",
			want:    false,
		},
		{
			name:    "no_results_to_judge",
			query:   "cartel de marca visual",
			topPath: "",
			want:    false,
		},
		{
			name:    "ordinary_code_query",
			query:   "classifyConsumptionJobDocument job transitions",
			topPath: "apps/brain/lib/inventory/consumption/job-transitions.ts",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := domainWarningFor(c.query, c.topPath)
			if (got != "") != c.want {
				t.Fatalf("domainWarningFor(%q, %q) = %q, want warning=%v", c.query, c.topPath, got, c.want)
			}
			if c.want && got != DomainWarningText {
				t.Errorf("unexpected warning text: %q", got)
			}
		})
	}
}

func TestCountVisualDomainTerms(t *testing.T) {
	cases := map[string]int{
		"Localizar activos visuales del cartel de marca": 3, // visual, cartel, marca
		"export the svg icono and the color palette":     3, // svg, icono, color palette
		"how does the ranker score filename matches":     0,
		"": 0,
	}
	for query, want := range cases {
		if got := countVisualDomainTerms(query); got != want {
			t.Errorf("countVisualDomainTerms(%q) = %d, want %d", query, got, want)
		}
	}
}

// TestSearchResponseCarriesDomainWarning drives the real MCP search tool
// against an indexed repo and asserts the served JSON carries the hint.
func TestSearchResponseCarriesDomainWarning(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	write := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(repo, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
	}
	// Only code in the index — exactly the shape that produced the "no"
	// ratings: the design assets the query wants are not indexable.
	write("apps/brain/lib/documents/identity.ts",
		"export function marcaVisualIdentity(cartel: string): string {\n  return cartel;\n}\n")

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
	if err := db.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}

	response, err := Search(context.Background(), SearchOptions{
		Query: "Localizar activos visuales del cartel de marca",
		Repo:  repo,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if response.DomainWarning != DomainWarningText {
		t.Fatalf("domain_warning = %q, want %q", response.DomainWarning, DomainWarningText)
	}
	if len(response.Results) == 0 {
		t.Error("the warning is a hint, not a block: results must still be served")
	}

	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"domain_warning"`) {
		t.Fatalf("serialized response is missing domain_warning: %s", payload)
	}

	codeQuery, err := Search(context.Background(), SearchOptions{
		Query: "marcaVisualIdentity",
		Repo:  repo,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if codeQuery.DomainWarning != "" {
		t.Fatalf("a code query must not be warned: %q", codeQuery.DomainWarning)
	}
}
