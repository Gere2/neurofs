package agentcontext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

func TestBuildPatchPromptIncludesLadderLogicAndMeasurement(t *testing.T) {
	repo, result := testRepoAndResult(t)

	agent, err := BuildPatchPrompt(repo, "sess123", result, Options{Transport: TransportCLI})
	if err != nil {
		t.Fatalf("agent prompt: %v", err)
	}
	for _, want := range []string{
		"<patch_context session=\"sess123\">",
		"<context_ladder>",
		"<editable_fragments>",
		"neurofs expand src/auth.go:3-5 --hash chunkhash --session sess123",
		"<logic path=\"src/auth.go\"",
		"related_tests:",
		"src/auth_test.go",
		"<measurement session=\"sess123\"",
	} {
		if !strings.Contains(agent.Text, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, agent.Text)
		}
	}
	if agent.InitialTokens <= 0 || agent.BaselineTokens <= 0 {
		t.Fatalf("expected token estimates, got %+v", agent)
	}
}

func TestBuildPatchPromptMCPUsesToolInstructions(t *testing.T) {
	repo, result := testRepoAndResult(t)

	result.Prompt = "<context>large original prompt body that MCP should not include</context>\n"
	agent, err := BuildPatchPrompt(repo, "sess123", result, Options{Transport: TransportMCP, Thin: true})
	if err != nil {
		t.Fatalf("agent prompt: %v", err)
	}
	for _, want := range []string{
		`call neurofs_expand`,
		`"target":"src/auth.go:3-5"`,
		`"hash":"chunkhash"`,
		`call neurofs_measure`,
		`"session_id":"sess123"`,
	} {
		if !strings.Contains(agent.Text, want) {
			t.Fatalf("mcp agent prompt missing %q:\n%s", want, agent.Text)
		}
	}
	if strings.Contains(agent.Text, "neurofs expand src/auth.go:3-5") {
		t.Fatalf("mcp prompt should prefer tool calls over shell commands:\n%s", agent.Text)
	}
	if strings.Contains(agent.Text, "large original prompt body") {
		t.Fatalf("thin MCP prompt should not include original bundle body:\n%s", agent.Text)
	}
	if !agent.Thin || len(agent.NextActions) == 0 {
		t.Fatalf("expected thin prompt with next actions, got %+v", agent)
	}
	foundExpand := false
	for _, action := range agent.NextActions {
		if action.Tool == "neurofs_expand" && action.Input["target"] == "src/auth.go:3-5" {
			foundExpand = true
		}
	}
	if !foundExpand {
		t.Fatalf("expected explicit range expansion action, got %+v", agent.NextActions)
	}
}

func testRepoAndResult(t *testing.T) (string, taskflow.Result) {
	t.Helper()
	repo := t.TempDir()
	srcDir := filepath.Join(repo, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	filePath := filepath.Join(srcDir, "auth.go")
	content := "package auth\n\nfunc VerifyJWT() bool {\n\treturn true\n}\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	testPath := filepath.Join(srcDir, "auth_test.go")
	if err := os.WriteFile(testPath, []byte("package auth\n"), 0o644); err != nil {
		t.Fatalf("write test: %v", err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	rec := models.FileRecord{
		Path:     filePath,
		RelPath:  "src/auth.go",
		Lang:     models.LangGo,
		Lines:    5,
		Checksum: "filehash",
	}
	testRec := models.FileRecord{
		Path:    testPath,
		RelPath: "src/auth_test.go",
		Lang:    models.LangGo,
	}
	if err := db.UpsertFile(rec); err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if err := db.UpsertFile(testRec); err != nil {
		t.Fatalf("upsert test: %v", err)
	}
	if err := db.UpdateChunks(filePath, []models.Chunk{{
		FilePath:      filePath,
		ChunkID:       "func-verifyjwt",
		Kind:          "func",
		Symbol:        "VerifyJWT",
		StartLine:     3,
		EndLine:       5,
		ContentHash:   "chunkhash",
		TokenEstimate: 20,
	}}); err != nil {
		t.Fatalf("chunks: %v", err)
	}

	result := taskflow.Result{
		Query:    "change jwt verification",
		RepoRoot: repo,
		Prompt:   "<context></context>\n",
		Bundle: models.Bundle{Fragments: []models.ContextFragment{{
			RelPath:        "src/auth.go",
			Representation: models.RepExcerpt,
			Tokens:         20,
			StartLine:      3,
			EndLine:        5,
			ContentHash:    "chunkhash",
		}}},
	}
	return repo, result
}
