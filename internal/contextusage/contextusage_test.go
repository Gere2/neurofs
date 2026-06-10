package contextusage

import (
	"testing"
)

func TestAppendReadAndSummarise(t *testing.T) {
	repo := t.TempDir()
	session := "s1"
	if err := Append(repo, Entry{
		SessionID:      session,
		Phase:          "initial_bundle",
		Command:        "task --agent",
		Tokens:         100,
		BaselineTokens: 500,
	}); err != nil {
		t.Fatalf("append initial: %v", err)
	}
	if err := Append(repo, Entry{
		SessionID: session,
		Phase:     "expansion",
		Command:   "expand",
		Path:      "src/auth.go",
		Mode:      "excerpt",
		StartLine: 10,
		EndLine:   20,
		Hash:      "hash1",
		Tokens:    40,
	}); err != nil {
		t.Fatalf("append expansion: %v", err)
	}
	if err := Append(repo, Entry{
		SessionID: session,
		Phase:     "expansion",
		Command:   "expand",
		Path:      "src/auth.go",
		Mode:      "full",
		Hash:      "filehash",
		Tokens:    120,
	}); err != nil {
		t.Fatalf("append full expansion: %v", err)
	}

	entries, err := Read(repo, session)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	summary := Summarise(session, entries, 0)
	if summary.InitialTokens != 100 || summary.ExpansionTokens != 160 || summary.TotalTokens != 260 {
		t.Fatalf("bad totals: %+v", summary)
	}
	if summary.Expansions != 2 || summary.EstimatedSaved != 240 {
		t.Fatalf("bad savings: %+v", summary)
	}
	if summary.ExpandedFiles != 1 || summary.FullFileExpansions != 1 || len(summary.Files) != 1 {
		t.Fatalf("bad file rollup: %+v", summary)
	}
	file := summary.Files[0]
	if file.Path != "src/auth.go" || file.Expansions != 2 || file.FullFileExpansions != 1 {
		t.Fatalf("bad file summary: %+v", file)
	}
	if len(file.Ranges) != 1 || file.Ranges[0] != "10-20" {
		t.Fatalf("bad ranges: %+v", file.Ranges)
	}
	if len(summary.OverExpandedFiles) != 1 || summary.OverExpandedFiles[0] != "src/auth.go" {
		t.Fatalf("expected over-expanded full-file marker: %+v", summary)
	}
}
