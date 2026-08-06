package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/grounding"
	"github.com/Gere2/neurofs/internal/models"
)

func ctxBundle() models.Bundle {
	return models.Bundle{
		Query:      "auth",
		BundleHash: "h",
		Fragments: []models.ContextFragment{
			{RelPath: "src/auth.ts", Content: "function verifyToken(){}", StartLine: 1, EndLine: 1},
		},
	}
}

func TestBuildGroundingEventEdit(t *testing.T) {
	repo := "/repo"
	ti, _ := json.Marshal(toolInput{FilePath: "/repo/src/auth.ts", NewString: "function verifyToken(){return 1}"})
	ev := hookEvent{HookEventName: "PostToolUse", ToolName: "Edit", CWD: repo, ToolInput: ti}
	got, ok, err := buildGroundingEvent(repo, ev, ctxBundle())
	if err != nil {
		t.Fatal(err)
	}
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
	if _, ok, err := buildGroundingEvent("/repo", ev, ctxBundle()); err != nil {
		t.Fatal(err)
	} else if ok {
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
	got, ok, err := buildGroundingEvent("/repo", ev, ctxBundle())
	if err != nil {
		t.Fatal(err)
	}
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
	if _, ok, err := buildGroundingEvent("/repo", ev, ctxBundle()); err != nil {
		t.Fatal(err)
	} else if ok {
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
	got, err := lastAssistantMessage(tp)
	if err != nil {
		t.Fatal(err)
	}
	if got != "second" {
		t.Fatalf("lastAssistantMessage = %q, want %q", got, "second")
	}
	got, err = lastAssistantMessage(filepath.Join(dir, "missing.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("missing transcript should yield empty, got %q", got)
	}
}

func TestGroundInputsAreBoundedAndUnsafeFilesRejected(t *testing.T) {
	if _, err := readGroundInput(strings.NewReader(strings.Repeat("x", int(maxHookEventBytes)+1)), maxHookEventBytes); err == nil {
		t.Fatal("oversized hook input was accepted")
	}

	dir := t.TempDir()
	transcript := filepath.Join(dir, "oversized.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Repeat("x", maxTranscriptLineBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lastAssistantMessage(transcript); err == nil {
		t.Fatal("oversized transcript line was accepted")
	}

	outside := filepath.Join(dir, "outside.bundle.json")
	link := filepath.Join(dir, "link.bundle.json")
	if err := os.WriteFile(outside, []byte(`{"query":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readBundleFile(link); err == nil {
		t.Fatal("symlinked grounding bundle was loaded")
	}
}
