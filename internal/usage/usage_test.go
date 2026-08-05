package usage

import (
	"os"
	"testing"
	"time"
)

func TestAppendFillsIDAndTimestamp(t *testing.T) {
	repo := t.TempDir()
	e, err := Append(repo, Entry{Source: "mcp", Tool: "neurofs_search", Query: "how does packing work"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if e.ID == "" {
		t.Error("ID not filled")
	}
	if e.Timestamp.IsZero() {
		t.Error("Timestamp not filled")
	}

	entries, err := Load(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != e.ID {
		t.Fatalf("load = %+v, want the appended entry", entries)
	}
}

func TestLoadFailsClosedOnCorruptLines(t *testing.T) {
	repo := t.TempDir()
	if _, err := Append(repo, Entry{Query: "good"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(Path(repo), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(repo, Entry{Query: "also good"}); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(repo); err == nil {
		t.Fatal("corrupt historical evidence was silently skipped")
	}
}

func TestUsageFilesRejectSymlinks(t *testing.T) {
	repo := t.TempDir()
	target := repo + "/target.jsonl"
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := Path(repo)
	if err := os.MkdirAll(repo+"/.neurofs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(repo, Entry{Query: "must not follow"}); err == nil {
		t.Fatal("append followed a symlink")
	}
	if _, err := Load(repo); err == nil {
		t.Fatal("load followed a symlink")
	}
}

func TestMatchEntryPrefersMostRecent(t *testing.T) {
	base := time.Now().UTC()
	entries := []Entry{
		{ID: "a", Query: "ranker weights", Timestamp: base},
		{ID: "b", Query: "other topic", Timestamp: base.Add(time.Minute)},
		{ID: "c", Query: "Ranker Weights", Timestamp: base.Add(2 * time.Minute)},
	}

	got, ok := MatchEntry(entries, "ranker weights")
	if !ok || got.ID != "c" {
		t.Fatalf("match by query = %+v ok=%v, want id c", got, ok)
	}

	got, ok = MatchEntry(entries, "")
	if !ok || got.ID != "c" {
		t.Fatalf("match empty query = %+v ok=%v, want most recent (c)", got, ok)
	}

	if _, ok := MatchEntry(entries, "no such query"); ok {
		t.Fatal("unexpected match for unknown query")
	}
}

func TestFeedbackRoundTrip(t *testing.T) {
	repo := t.TempDir()
	err := AppendFeedback(repo, Feedback{
		UsageID:       "abc123",
		Query:         "ranker weights",
		Rating:        RatingPartial,
		UsefulSymbols: []string{"weightFilename"},
		MissingFacts:  []string{"scoreFile"},
	})
	if err != nil {
		t.Fatalf("append feedback: %v", err)
	}
	fbs, err := LoadFeedback(repo)
	if err != nil {
		t.Fatalf("load feedback: %v", err)
	}
	if len(fbs) != 1 || fbs[0].Rating != RatingPartial || fbs[0].UsageID != "abc123" {
		t.Fatalf("feedback = %+v", fbs)
	}
}
