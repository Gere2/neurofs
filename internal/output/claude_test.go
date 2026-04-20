package output_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
)

func TestWriteClaudeStructure(t *testing.T) {
	b := models.Bundle{
		Query:  "why does ranking pick utils.py",
		Budget: 3000,
		Fragments: []models.ContextFragment{{
			RelPath:        "internal/ranking/ranking.go",
			Lang:           models.LangGo,
			Representation: models.RepSignature,
			Tokens:         42,
			Content:        "func Rank(files, query) []ScoredFile",
			Reasons: []models.InclusionReason{
				{Signal: "filename_match", Detail: "ranking", Weight: 3.0},
				{Signal: "focus", Detail: "internal/ranking", Weight: 4.0},
			},
		}},
		Stats: models.BundleStats{
			FilesConsidered:  8,
			FilesIncluded:    1,
			TokensUsed:       120,
			TokensBudget:     3000,
			CompressionRatio: 3.2,
		},
	}
	summary := output.RepoSummary{
		Name:      "neurofs",
		Files:     42,
		Symbols:   200,
		Entry:     "cmd/neurofs/main.go",
		Languages: map[string]int{"go": 30, "typescript": 8, "python": 4},
	}

	var buf bytes.Buffer
	if err := output.WriteClaude(&buf, b, summary); err != nil {
		t.Fatalf("WriteClaude: %v", err)
	}
	got := buf.String()

	// Hard requirements: the exact shape Claude needs to ground on.
	for _, want := range []string{
		"<task>", "</task>",
		"why does ranking pick utils.py",
		"<repo>", "name: neurofs", "languages:", "go:30",
		"<selection>", "bundle:",
		"<context>", `path="internal/ranking/ranking.go"`, `rep="signature"`,
		`reasons="focus,filename_match"`, // sorted by aggregate weight
		"<instructions>", "cite it as `path:line`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("claude output missing %q\n---\n%s", want, got)
		}
	}

	// Repo tag must close exactly once.
	if strings.Count(got, "</repo>") != 1 {
		t.Errorf("expected one </repo>, output:\n%s", got)
	}
}

func TestWriteClaudeSkipsEmptyRepoSummary(t *testing.T) {
	b := models.Bundle{Query: "x"}
	var buf bytes.Buffer
	if err := output.WriteClaude(&buf, b, output.RepoSummary{}); err != nil {
		t.Fatalf("WriteClaude: %v", err)
	}
	if strings.Contains(buf.String(), "<repo>") {
		t.Errorf("empty summary should not emit <repo> block:\n%s", buf.String())
	}
}

func TestWriteClaudeThroughDispatcher(t *testing.T) {
	// output.Write with FormatClaude must not panic and must produce a
	// task-first body even without a summary.
	var buf bytes.Buffer
	err := output.Write(&buf, models.Bundle{Query: "q"}, output.FormatClaude)
	if err != nil {
		t.Fatalf("Write(FormatClaude): %v", err)
	}
	if !strings.HasPrefix(buf.String(), "<task>") {
		t.Errorf("claude dispatcher should lead with <task>, got: %s", buf.String())
	}
}
