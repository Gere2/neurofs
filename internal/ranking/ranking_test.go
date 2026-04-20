package ranking_test

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
)

func TestRankByFilename(t *testing.T) {
	files := []models.FileRecord{
		{RelPath: "src/products.ts", Lang: models.LangTypeScript},
		{RelPath: "src/auth.ts", Lang: models.LangTypeScript},
		{RelPath: "src/utils.py", Lang: models.LangPython},
	}

	ranked := ranking.Rank(files, "authentication login")

	// auth.ts must come before products.ts and utils.py
	if len(ranked) < 2 {
		t.Fatal("expected ranked results")
	}
	if ranked[0].Record.RelPath != "src/auth.ts" {
		t.Errorf("expected auth.ts first, got %s", ranked[0].Record.RelPath)
	}
	if ranked[0].Score <= ranked[1].Score {
		t.Errorf("auth.ts score %.2f should be > %s score %.2f",
			ranked[0].Score, ranked[1].Record.RelPath, ranked[1].Score)
	}
}

func TestRankBySymbol(t *testing.T) {
	files := []models.FileRecord{
		{
			RelPath: "src/crypto.ts",
			Lang:    models.LangTypeScript,
			Symbols: []models.Symbol{
				{Name: "hashPassword", Kind: "export_func"},
				{Name: "comparePassword", Kind: "export_func"},
			},
		},
		{
			RelPath: "src/api.ts",
			Lang:    models.LangTypeScript,
			Symbols: []models.Symbol{
				{Name: "router", Kind: "const"},
			},
		},
	}

	ranked := ranking.Rank(files, "password hashing")

	if ranked[0].Record.RelPath != "src/crypto.ts" {
		t.Errorf("expected crypto.ts first, got %s", ranked[0].Record.RelPath)
	}
}

func TestRankEmptyQuery(t *testing.T) {
	files := []models.FileRecord{
		{RelPath: "a.ts"},
		{RelPath: "b.ts"},
	}
	ranked := ranking.Rank(files, "")
	if len(ranked) != 2 {
		t.Errorf("expected 2 results, got %d", len(ranked))
	}
}

func TestRankEmptyIndex(t *testing.T) {
	ranked := ranking.Rank(nil, "anything")
	if len(ranked) != 0 {
		t.Errorf("expected 0 results for empty index")
	}
}

func TestRankEntryPointBonus(t *testing.T) {
	files := []models.FileRecord{
		{RelPath: "src/api.ts", Lang: models.LangTypeScript},
		{RelPath: "src/helper.ts", Lang: models.LangTypeScript},
	}
	info := &project.Info{Main: "src/api.ts"}

	ranked := ranking.RankWithOptions(files, "api handler", ranking.Options{Project: info})

	if ranked[0].Record.RelPath != "src/api.ts" {
		t.Errorf("expected entry point first, got %s", ranked[0].Record.RelPath)
	}
	if !hasSignal(ranked[0].Reasons, "entry_point") {
		t.Errorf("expected entry_point reason, got %+v", ranked[0].Reasons)
	}
}

func TestRankDependencyMatch(t *testing.T) {
	files := []models.FileRecord{
		{
			RelPath: "src/crypto.ts",
			Lang:    models.LangTypeScript,
			Imports: []string{"bcrypt"},
		},
		{
			RelPath: "src/util.ts",
			Lang:    models.LangTypeScript,
		},
	}
	info := &project.Info{Dependencies: []string{"bcrypt", "express"}}

	ranked := ranking.RankWithOptions(files, "bcrypt password", ranking.Options{Project: info})

	if ranked[0].Record.RelPath != "src/crypto.ts" {
		t.Errorf("expected crypto.ts first, got %s", ranked[0].Record.RelPath)
	}
	if !hasSignal(ranked[0].Reasons, "dependency_match") {
		t.Errorf("expected dependency_match reason on crypto.ts, got %+v", ranked[0].Reasons)
	}
}

func TestRankPathAliasExpansion(t *testing.T) {
	// When tsconfig maps @app/* → src/*, a file under src/user should still
	// receive the import_expansion boost from a seed importing @app/user.
	files := []models.FileRecord{
		{
			RelPath: "src/api.ts",
			Lang:    models.LangTypeScript,
			Symbols: []models.Symbol{{Name: "router", Kind: "const"}},
			Imports: []string{"@app/user"},
		},
		{
			RelPath: "src/user.ts",
			Lang:    models.LangTypeScript,
		},
	}
	info := &project.Info{
		PathAliases: map[string]string{"@app": "src"},
	}

	ranked := ranking.RankWithOptions(files, "router endpoint", ranking.Options{Project: info})

	var userFile *models.ScoredFile
	for i := range ranked {
		if ranked[i].Record.RelPath == "src/user.ts" {
			userFile = &ranked[i]
			break
		}
	}
	if userFile == nil {
		t.Fatal("user.ts missing from ranked output")
	}
	if !hasSignal(userFile.Reasons, "import_expansion") {
		t.Errorf("expected import_expansion on src/user.ts via @app alias, got %+v", userFile.Reasons)
	}
}

func TestRankReasonsPopulated(t *testing.T) {
	files := []models.FileRecord{
		{
			RelPath: "src/auth/middleware.ts",
			Lang:    models.LangTypeScript,
			Symbols: []models.Symbol{{Name: "AuthMiddleware", Kind: "export_class"}},
		},
	}

	ranked := ranking.Rank(files, "auth middleware")

	if len(ranked) == 0 {
		t.Fatal("expected ranked results")
	}
	if len(ranked[0].Reasons) == 0 {
		t.Error("expected reasons to be populated")
	}

	signals := make(map[string]bool)
	for _, r := range ranked[0].Reasons {
		signals[r.Signal] = true
	}
	if !signals["filename_match"] && !signals["path_match"] && !signals["symbol_match"] {
		t.Errorf("expected at least one structural signal, got %v", signals)
	}
}

func TestRankStemmingUtilityToUtils(t *testing.T) {
	// The bench failure we are fixing: "utility" in the query should match
	// a file named utils.py via shared stem "util".
	files := []models.FileRecord{
		{RelPath: "src/utils.py", Lang: models.LangPython},
		{RelPath: "src/auth.ts", Lang: models.LangTypeScript},
		{RelPath: "src/api.ts", Lang: models.LangTypeScript},
	}

	ranked := ranking.Rank(files, "what utility helpers are available in python")

	if ranked[0].Record.RelPath != "src/utils.py" {
		t.Errorf("expected utils.py first via stemming, got %s", ranked[0].Record.RelPath)
	}
	if !hasSignal(ranked[0].Reasons, "filename_match") {
		t.Errorf("expected filename_match reason on utils.py, got %+v", ranked[0].Reasons)
	}
}

func TestRankStemmingReverse(t *testing.T) {
	// Inverse direction: a short file basename should still match a longer
	// query term (utils.py → "utility"). This exercises the variant path
	// rather than the reverse-containment branch.
	files := []models.FileRecord{
		{RelPath: "src/util.py", Lang: models.LangPython},
	}

	ranked := ranking.Rank(files, "utility functions")
	if len(ranked) == 0 || ranked[0].Score < 3.0 {
		t.Errorf("expected utility→util filename match, got %+v", ranked)
	}
}

func TestRankStemmingPlurals(t *testing.T) {
	// "queries" should match a symbol named buildQuery via stem "query".
	files := []models.FileRecord{
		{
			RelPath: "src/db.ts",
			Lang:    models.LangTypeScript,
			Symbols: []models.Symbol{{Name: "buildQuery", Kind: "export_func"}},
		},
		{RelPath: "src/other.ts", Lang: models.LangTypeScript},
	}

	ranked := ranking.Rank(files, "how do we build queries")
	if ranked[0].Record.RelPath != "src/db.ts" {
		t.Errorf("expected db.ts first via queries→query stem, got %s", ranked[0].Record.RelPath)
	}
	if !hasSignal(ranked[0].Reasons, "symbol_match") {
		t.Errorf("expected symbol_match via stemming, got %+v", ranked[0].Reasons)
	}
}

func TestRankFocusBoost(t *testing.T) {
	files := []models.FileRecord{
		{RelPath: "docs/architecture.md", Lang: models.LangMarkdown},
		{RelPath: "internal/ranking/ranking.go", Lang: models.LangGo},
		{RelPath: "internal/packager/packager.go", Lang: models.LangGo},
	}
	ranked := ranking.RankWithOptions(files, "scoring", ranking.Options{
		Focus: "internal/ranking",
	})
	if ranked[0].Record.RelPath != "internal/ranking/ranking.go" {
		t.Errorf("focus should float internal/ranking to the top, got %s", ranked[0].Record.RelPath)
	}
	if !hasSignal(ranked[0].Reasons, "focus") {
		t.Errorf("focus reason missing: %+v", ranked[0].Reasons)
	}
}

func TestRankFocusMultiplePrefixes(t *testing.T) {
	files := []models.FileRecord{
		{RelPath: "internal/audit/audit.go", Lang: models.LangGo},
		{RelPath: "internal/ranking/ranking.go", Lang: models.LangGo},
		{RelPath: "cmd/neurofs/main.go", Lang: models.LangGo},
	}
	ranked := ranking.RankWithOptions(files, "anything", ranking.Options{
		Focus: "internal/audit, internal/ranking",
	})
	// Both focus targets should outrank the non-matching file.
	if ranked[2].Record.RelPath != "cmd/neurofs/main.go" {
		t.Errorf("expected cmd/neurofs/main.go last, got ranking %+v", ranked)
	}
}

func TestRankChangedBoost(t *testing.T) {
	files := []models.FileRecord{
		{RelPath: "src/auth.ts", Lang: models.LangTypeScript},
		{RelPath: "src/utils.py", Lang: models.LangPython},
	}
	ranked := ranking.RankWithOptions(files, "", ranking.Options{
		ChangedFiles: []string{"src/utils.py"},
	})
	if ranked[0].Record.RelPath != "src/utils.py" {
		t.Errorf("changed file should rank first with empty query, got %s", ranked[0].Record.RelPath)
	}
	if !hasSignal(ranked[0].Reasons, "changed") {
		t.Errorf("changed reason missing: %+v", ranked[0].Reasons)
	}
}

func hasSignal(reasons []models.InclusionReason, signal string) bool {
	for _, r := range reasons {
		if r.Signal == signal {
			return true
		}
	}
	return false
}
