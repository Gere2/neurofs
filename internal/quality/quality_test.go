package quality

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAppendCreatesFileAndDir(t *testing.T) {
	dir := t.TempDir()
	e := Entry{
		Query:         "where is jwt verified",
		Repo:          dir,
		TokensUsed:    1234,
		TokensBudget:  3500,
		FilesIncluded: 5,
		TopPicks:      []string{"internal/auth/jwt.go", "internal/middleware/auth.go"},
		Rating:        RatingYes,
		Comment:       "missed config.go",
	}
	if err := Append(dir, e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected newline-terminated JSONL, got %q", data)
	}

	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Query != e.Query || got.Rating != e.Rating || len(got.TopPicks) != 2 {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
	if got.Timestamp.IsZero() {
		t.Fatalf("expected Timestamp to be auto-filled")
	}
}

func TestAppendIsAppendOnly(t *testing.T) {
	dir := t.TempDir()
	for i, q := range []string{"first", "second", "third"} {
		if err := Append(dir, Entry{Query: q, Rating: RatingNo}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	f, err := os.Open(Path(dir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	for i, want := range []string{"first", "second", "third"} {
		var e Entry
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			t.Fatalf("line %d decode: %v", i, err)
		}
		if e.Query != want {
			t.Fatalf("line %d query: got %q want %q", i, e.Query, want)
		}
	}
}
