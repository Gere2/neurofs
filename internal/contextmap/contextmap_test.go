package contextmap

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestBuildLogicMapIncludesRangesCallsRelationsAndTests(t *testing.T) {
	rec := models.FileRecord{
		Path:    "/repo/src/auth.go",
		RelPath: "src/auth.go",
		Lang:    models.LangGo,
		Lines:   8,
		Imports: []string{"crypto"},
	}
	testRec := models.FileRecord{Path: "/repo/src/auth_test.go", RelPath: "src/auth_test.go", Lang: models.LangGo}
	depRec := models.FileRecord{Path: "/repo/src/crypto.go", RelPath: "src/crypto.go", Lang: models.LangGo}
	consumer := models.FileRecord{Path: "/repo/src/server.go", RelPath: "src/server.go", Lang: models.LangGo}
	files := []models.FileRecord{rec, testRec, depRec, consumer}
	chunks := []models.Chunk{{
		FilePath:      rec.Path,
		Kind:          "func",
		Symbol:        "VerifyJWT",
		StartLine:     3,
		EndLine:       7,
		ContentHash:   "hash-auth",
		TokenEstimate: 22,
	}}
	rels := []models.FileRelation{
		{SourcePath: rec.Path, TargetPath: depRec.Path, RelType: "import"},
		{SourcePath: consumer.Path, TargetPath: rec.Path, RelType: "import"},
		{SourcePath: testRec.Path, TargetPath: rec.Path, RelType: "import"},
	}
	content := "package auth\n\nfunc VerifyJWT() bool {\n\tparseToken()\n\tcrypto.Check()\n\treturn true\n}\n"

	logic := Build(rec, files, chunks, rels, content)
	if len(logic.Symbols) != 1 {
		t.Fatalf("expected one symbol, got %+v", logic.Symbols)
	}
	sym := logic.Symbols[0]
	if sym.StartLine != 3 || sym.EndLine != 7 || sym.ContentHash != "hash-auth" {
		t.Fatalf("symbol range/hash missing: %+v", sym)
	}
	if !contains(sym.Calls, "parseToken") || !contains(sym.Calls, "crypto.Check") {
		t.Fatalf("calls missing: %+v", sym.Calls)
	}
	if !contains(logic.Dependencies, "src/crypto.go") {
		t.Fatalf("dependency missing: %+v", logic.Dependencies)
	}
	if !contains(logic.Dependents, "src/server.go") || !contains(logic.Dependents, "src/auth_test.go") {
		t.Fatalf("dependents missing: %+v", logic.Dependents)
	}
	if !contains(logic.RelatedTests, "src/auth_test.go") {
		t.Fatalf("related test missing: %+v", logic.RelatedTests)
	}
}

func TestLinesInRange(t *testing.T) {
	got := LinesInRange("a\nb\nc\n", 2, 3)
	if got != "b\nc" {
		t.Fatalf("unexpected range: %q", got)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
