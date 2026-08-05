package fixturehistory

import "testing"

func TestActiveSupportsAppendOnlyCorrectionChains(t *testing.T) {
	active, err := Active([]Entry{
		{Name: "old.json"},
		{Name: "correction.json", Supersedes: "old.json"},
		{Name: "latest.json", Supersedes: "correction.json"},
		{Name: "unrelated.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || !active["latest.json"] || !active["unrelated.json"] {
		t.Fatalf("active fixtures = %v", active)
	}
}

func TestActiveRejectsUnsafeOrAmbiguousHistory(t *testing.T) {
	tests := map[string][]Entry{
		"missing target": {
			{Name: "correction.json", Supersedes: "missing.json"},
		},
		"path target": {
			{Name: "old.json"},
			{Name: "correction.json", Supersedes: "../old.json"},
		},
		"duplicate correction": {
			{Name: "old.json"},
			{Name: "a.json", Supersedes: "old.json"},
			{Name: "b.json", Supersedes: "old.json"},
		},
		"cycle": {
			{Name: "a.json", Supersedes: "b.json"},
			{Name: "b.json", Supersedes: "a.json"},
		},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Active(entries); err == nil {
				t.Fatal("invalid fixture history was accepted")
			}
		})
	}
}
