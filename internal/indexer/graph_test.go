package indexer

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestBuildRelationsRelativeTSImport(t *testing.T) {
	files := []models.FileRecord{
		{Path: "/repo/src/main.ts", RelPath: "src/main.ts", Lang: models.LangTypeScript,
			Imports: []string{"./helper"}},
		{Path: "/repo/src/helper.ts", RelPath: "src/helper.ts", Lang: models.LangTypeScript},
	}
	rels := BuildRelations(files)
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation, got %d (%v)", len(rels), rels)
	}
	r := rels[0]
	if r.SourcePath != "/repo/src/main.ts" || r.TargetPath != "/repo/src/helper.ts" {
		t.Errorf("unexpected relation paths: %+v", r)
	}
	if r.RelType != "import" {
		t.Errorf("expected RelType=import, got %q", r.RelType)
	}
}

func TestBuildRelationsGoPackageSuffixMatch(t *testing.T) {
	files := []models.FileRecord{
		{Path: "/repo/cmd/app/main.go", RelPath: "cmd/app/main.go", Lang: models.LangGo,
			Imports: []string{"github.com/example/proj/internal/storage"}},
		{Path: "/repo/internal/storage/storage.go", RelPath: "internal/storage/storage.go", Lang: models.LangGo},
	}
	rels := BuildRelations(files)
	found := false
	for _, r := range rels {
		if r.SourcePath == "/repo/cmd/app/main.go" && r.TargetPath == "/repo/internal/storage/storage.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected main.go → storage.go via package suffix match, got %v", rels)
	}
}

func TestBuildRelationsFolderIndexImport(t *testing.T) {
	// import "./user" should resolve to "./user/index.ts"
	files := []models.FileRecord{
		{Path: "/repo/src/main.ts", RelPath: "src/main.ts", Lang: models.LangTypeScript,
			Imports: []string{"./user"}},
		{Path: "/repo/src/user/index.ts", RelPath: "src/user/index.ts", Lang: models.LangTypeScript},
	}
	rels := BuildRelations(files)
	if len(rels) != 1 || rels[0].TargetPath != "/repo/src/user/index.ts" {
		t.Errorf("expected main.ts → src/user/index.ts via folder/index resolution; got %v", rels)
	}
}

func TestBuildRelationsExtensionResolution(t *testing.T) {
	// import "./helper" must try ".ts", ".js", ".tsx", ".jsx", ".go", ".py"
	files := []models.FileRecord{
		{Path: "/repo/main.py", RelPath: "main.py", Lang: models.LangPython,
			Imports: []string{"./util"}},
		{Path: "/repo/util.py", RelPath: "util.py", Lang: models.LangPython},
	}
	rels := BuildRelations(files)
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(rels))
	}
	if rels[0].TargetPath != "/repo/util.py" {
		t.Errorf("expected util.py target, got %v", rels[0])
	}
}

func TestBuildRelationsSkipsSelfAndDedupes(t *testing.T) {
	files := []models.FileRecord{
		{Path: "/repo/a.ts", RelPath: "a.ts", Lang: models.LangTypeScript,
			Imports: []string{"./b", "./b", "./a"}}, // ./b twice + self-import
		{Path: "/repo/b.ts", RelPath: "b.ts", Lang: models.LangTypeScript},
	}
	rels := BuildRelations(files)
	if len(rels) != 1 {
		t.Errorf("expected exactly 1 deduped relation (self-imports skipped), got %d: %v", len(rels), rels)
	}
}

func TestBuildRelationsEmptyImports(t *testing.T) {
	files := []models.FileRecord{
		{Path: "/repo/a.ts", RelPath: "a.ts", Lang: models.LangTypeScript, Imports: nil},
		{Path: "/repo/b.ts", RelPath: "b.ts", Lang: models.LangTypeScript, Imports: []string{"  ", ""}},
	}
	rels := BuildRelations(files)
	if len(rels) != 0 {
		t.Errorf("expected 0 relations from empty/whitespace imports, got %d: %v", len(rels), rels)
	}
}

func TestBuildRelationsUnresolvedImport(t *testing.T) {
	// import to non-existent file: should not produce a relation
	files := []models.FileRecord{
		{Path: "/repo/a.ts", RelPath: "a.ts", Lang: models.LangTypeScript,
			Imports: []string{"./nonexistent", "external-lib"}},
	}
	rels := BuildRelations(files)
	if len(rels) != 0 {
		t.Errorf("expected 0 relations when imports cannot be resolved, got %v", rels)
	}
}
