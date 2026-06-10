package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuromfs/neuromfs/internal/grounding"
	"github.com/neuromfs/neuromfs/internal/models"
)

func ctxBundle() models.Bundle {
	return models.Bundle{
		Query:      "auth",
		BundleHash: "h",
		Fragments: []models.ContextFragment{
			{RelPath: "src/auth.ts", Content: "function verifyToken(){}"},
		},
	}
}

func TestBuildGroundingEventEdit(t *testing.T) {
	repo := "/repo"
	ti, _ := json.Marshal(toolInput{FilePath: "/repo/src/auth.ts", NewString: "function verifyToken(){return 1}"})
	ev := hookEvent{HookEventName: "PostToolUse", ToolName: "Edit", CWD: repo, ToolInput: ti}
	got, ok := buildGroundingEvent(repo, ev, ctxBundle())
	if !ok {
		t.Fatal("edit event should be actionable")
	}
	if got.Kind != grounding.KindEdit {
		t.Fatalf("kind = %q", got.Kind)
	}
	if got.FileInContext == nil || !*got.FileInContext {
		t.Fatalf("src/auth.ts is in the bundle; expected FileInContext true")
	}
	if got.Origin != "PostToolUse:Edit" {
		t.Fatalf("origin = %q", got.Origin)
	}
}

func TestBuildGroundingEventNonActionableTool(t *testing.T) {
	ev := hookEvent{HookEventName: "PostToolUse", ToolName: "Bash"}
	if _, ok := buildGroundingEvent("/repo", ev, ctxBundle()); ok {
		t.Fatal("a Bash tool event should not be recorded as grounding")
	}
}

func TestBuildGroundingEventStopFromTranscript(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Auth is in src/auth.ts:1 via verifyToken."}]}}
`
	if err := os.WriteFile(tp, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := hookEvent{HookEventName: "Stop", TranscriptPath: tp}
	got, ok := buildGroundingEvent("/repo", ev, ctxBundle())
	if !ok {
		t.Fatal("stop event with a transcript should be actionable")
	}
	if got.Kind != grounding.KindResponse {
		t.Fatalf("kind = %q", got.Kind)
	}
	if got.GroundedRatio < 0.999 {
		t.Fatalf("grounded = %.2f, want 1.0 (cited src/auth.ts is in the bundle)", got.GroundedRatio)
	}
}

func TestBuildGroundingEventStopEmptyTranscript(t *testing.T) {
	ev := hookEvent{HookEventName: "Stop", TranscriptPath: ""}
	if _, ok := buildGroundingEvent("/repo", ev, ctxBundle()); ok {
		t.Fatal("a Stop with no recoverable response should be skipped, not recorded")
	}
}

func TestToRepoRel(t *testing.T) {
	cases := []struct {
		repo, cwd, in, want string
	}{
		{"/repo", "/repo", "/repo/src/a.go", "src/a.go"},
		{"/repo", "/repo", "src/a.go", "src/a.go"},
		{"/repo", "/repo/sub", "a.go", "sub/a.go"},
	}
	for _, c := range cases {
		if got := toRepoRel(c.repo, c.cwd, c.in); got != c.want {
			t.Fatalf("toRepoRel(%q,%q,%q) = %q, want %q", c.repo, c.cwd, c.in, got, c.want)
		}
	}
}

func TestLastAssistantMessageTakesLast(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}
`
	if err := os.WriteFile(tp, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastAssistantMessage(tp); got != "second" {
		t.Fatalf("lastAssistantMessage = %q, want %q", got, "second")
	}
	if got := lastAssistantMessage(filepath.Join(dir, "missing.jsonl")); got != "" {
		t.Fatalf("missing transcript should yield empty, got %q", got)
	}
}
