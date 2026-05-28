package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDiff_NoRegressionsWhenIdentical(t *testing.T) {
	r := Report{
		Criteria: []Criterion{{ID: "G1", Verdict: Pass}, {ID: "G3", Verdict: Pass}},
		G3Details: []FactResult{
			{Fixture: Fixture{Question: "q1", SourcePath: "f1.json"}, Recall: 1.0},
		},
	}
	if got := Diff(r, r); len(got) != 0 {
		t.Fatalf("identical reports must not regress; got %+v", got)
	}
}

func TestDiff_VerdictDowngradeIsRegression(t *testing.T) {
	base := Report{Criteria: []Criterion{{ID: "G3", Name: "Fact recovery", Verdict: Pass}}}
	curr := Report{Criteria: []Criterion{{ID: "G3", Name: "Fact recovery", Verdict: Fail}}}
	got := Diff(curr, base)
	if len(got) != 1 || got[0].Kind != "verdict_downgrade" {
		t.Fatalf("expected one verdict_downgrade; got %+v", got)
	}
	if got[0].Before != "PASS" || got[0].After != "FAIL" {
		t.Errorf("before/after wrong: %s → %s", got[0].Before, got[0].After)
	}
}

func TestDiff_PassToSkipIsNotRegression(t *testing.T) {
	base := Report{Criteria: []Criterion{{ID: "G3", Verdict: Pass}}}
	curr := Report{Criteria: []Criterion{{ID: "G3", Verdict: Skip}}}
	if got := Diff(curr, base); len(got) != 0 {
		t.Fatalf("PASS→SKIP must NOT be a regression (too noisy as PR-blocker); got %+v", got)
	}
}

func TestDiff_FixtureThatWentFromPerfectToImperfectFails(t *testing.T) {
	base := Report{G3Details: []FactResult{
		{Fixture: Fixture{Question: "where is jwt verified", SourcePath: "audit/facts/jwt.json"}, Recall: 1.0},
	}}
	curr := Report{G3Details: []FactResult{
		{Fixture: Fixture{Question: "where is jwt verified", SourcePath: "audit/facts/jwt.json"}, Recall: 0.5, Misses: []string{"jwt.Verify"}},
	}}
	got := Diff(curr, base)
	if len(got) != 1 || got[0].Kind != "fixture_failed" {
		t.Fatalf("expected fixture_failed; got %+v", got)
	}
	if got[0].Where != "audit/facts/jwt.json" {
		t.Errorf("Where should be the source path for GHA annotations; got %q", got[0].Where)
	}
}

func TestDiff_RecallDropOver5pp(t *testing.T) {
	base := Report{G3Details: []FactResult{
		{Fixture: Fixture{Question: "q", SourcePath: "f.json"}, Recall: 0.85},
	}}
	curr := Report{G3Details: []FactResult{
		{Fixture: Fixture{Question: "q", SourcePath: "f.json"}, Recall: 0.70},
	}}
	got := Diff(curr, base)
	if len(got) != 1 || got[0].Kind != "recall_dropped" {
		t.Fatalf("expected recall_dropped; got %+v", got)
	}
}

func TestDiff_NewFixtureIsNotRegression(t *testing.T) {
	base := Report{G3Details: []FactResult{}}
	curr := Report{G3Details: []FactResult{
		{Fixture: Fixture{Question: "new question"}, Recall: 0.5},
	}}
	if got := Diff(curr, base); len(got) != 0 {
		t.Fatalf("adding a fixture cannot regress; got %+v", got)
	}
}

func TestLoadBaseline_RoundTrip(t *testing.T) {
	r := Report{
		Criteria: []Criterion{{ID: "G1", Verdict: Pass, Detail: "ok"}},
		G3Details: []FactResult{
			{Fixture: Fixture{Question: "q1", SourcePath: "f.json"}, Recall: 1.0},
		},
		Overall: Pass,
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Overall != Pass || len(loaded.G3Details) != 1 {
		t.Errorf("round-trip lost data: %+v", loaded)
	}
	if loaded.G3Details[0].Fixture.SourcePath != "f.json" {
		t.Errorf("source_path must round-trip via JSON for GHA annotations; got %q", loaded.G3Details[0].Fixture.SourcePath)
	}
}
