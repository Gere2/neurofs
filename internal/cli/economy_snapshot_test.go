package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

func TestEconomyRefreshesBeforeCapturingOneIndexGeneration(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	t.Setenv("NEUROFS_MOCK_SEMANTIC", "")

	repo := t.TempDir()
	sourcePath := filepath.Join(repo, "answer.go")
	if err := os.WriteFile(
		sourcePath,
		[]byte("package answer\n\nconst OldFact = \"old\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Leave the persisted index on OldFact. Economy must refresh before it
	// captures both the search session and native FileRecords.
	if err := os.WriteFile(
		sourcePath,
		[]byte("package answer\n\nconst NewFact = \"new\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	fixturesDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(fixturesDir, "new-fact.json"),
		[]byte(`{"question":"Where is NewFact defined?","expects_facts":["NewFact"]}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cmd := newEconomyCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{
		"--repo", repo,
		"--fixtures-dir", fixturesDir,
		"--json",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("economy: %v\n%s", err, stdout.String())
	}
	var report economyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if len(report.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(report.Tasks))
	}
	task := report.Tasks[0]
	if !task.Scored || task.Neurofs.Recall != 1 || task.NativeIso.Recall != 1 {
		t.Fatalf("mixed or stale economy generation: %+v", task)
	}
}
