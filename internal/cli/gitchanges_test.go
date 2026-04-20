package cli

import (
	"reflect"
	"testing"
)

func TestParsePorcelainCoversAllStates(t *testing.T) {
	// Mixed porcelain output: modified, staged-new, untracked, renamed.
	// Rename lines encode "old -> new" in the path portion; we keep new.
	input := []byte(
		" M internal/ranking/ranking.go\n" +
			"?? scratch.md\n" +
			"A  internal/audit/new.go\n" +
			"R  old.go -> internal/audit/renamed.go\n" +
			" M \"spaced path.go\"\n",
	)
	got := parsePorcelain(input)
	want := []string{
		"internal/ranking/ranking.go",
		"scratch.md",
		"internal/audit/new.go",
		"internal/audit/renamed.go",
		"spaced path.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePorcelain mismatch\n got:  %v\n want: %v", got, want)
	}
}

func TestParsePorcelainEmptyAndShortLines(t *testing.T) {
	if out := parsePorcelain(nil); out != nil {
		t.Errorf("nil input should return nil, got %v", out)
	}
	// Lines shorter than 4 chars are skipped (format always needs 2 status
	// columns + space + path).
	if out := parsePorcelain([]byte("XY\n")); out != nil {
		t.Errorf("short line should be ignored, got %v", out)
	}
}

func TestParsePorcelainDeduplicates(t *testing.T) {
	// Git can list a file twice (staged + unstaged): same path repeated.
	input := []byte("MM internal/ranking/ranking.go\n M internal/ranking/ranking.go\n")
	got := parsePorcelain(input)
	if len(got) != 1 || got[0] != "internal/ranking/ranking.go" {
		t.Errorf("expected single dedup'd path, got %v", got)
	}
}
