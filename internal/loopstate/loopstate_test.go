package loopstate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/agentcontext"
	"github.com/neuromfs/neuromfs/internal/grounding"
	"github.com/neuromfs/neuromfs/internal/memory"
	"github.com/neuromfs/neuromfs/internal/models"
)

const sess = "sess-test"

func seed(t *testing.T, repo string, e models.LedgerEntry) {
	t.Helper()
	e.SessionID = sess
	if err := memory.AppendEntry(repo, e); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestDigestFreshSession(t *testing.T) {
	st, err := Digest(t.TempDir(), sess)
	if err != nil {
		t.Fatalf("fresh session should not error: %v", err)
	}
	if len(st.Attempts) != 0 || len(st.PendingNextActions) != 0 {
		t.Fatalf("fresh session should be empty, got %+v", st)
	}
	if st.Summary == "" {
		t.Fatalf("expected a summary even for a fresh session")
	}
}

func TestRecordAndRecallNextActions(t *testing.T) {
	repo := t.TempDir()
	actions := []agentcontext.NextAction{
		{Tool: "neurofs_get_outline", Input: map[string]any{"path": "src/auth.ts"}, Reason: "inspect symbols"},
		{Tool: "neurofs_expand", Input: map[string]any{"target": "src/user.ts:1-20"}, Reason: "read range"},
	}
	if err := RecordNextActions(repo, sess, "how does auth work", actions); err != nil {
		t.Fatal(err)
	}
	st, err := Digest(repo, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingNextActions) != 2 {
		t.Fatalf("pending next actions = %d, want 2", len(st.PendingNextActions))
	}
	if st.PendingNextActions[0].Tool != "neurofs_get_outline" {
		t.Fatalf("first action = %q", st.PendingNextActions[0].Tool)
	}
}

func TestPendingFilteredByLaterEdit(t *testing.T) {
	repo := t.TempDir()
	base := time.Now().UTC().Add(-time.Hour)

	// next_actions targeting two files, recorded at base.
	actions := []agentcontext.NextAction{
		{Tool: "neurofs_get_outline", Input: map[string]any{"path": "src/auth.ts"}},
		{Tool: "neurofs_expand", Input: map[string]any{"target": "src/user.ts:1-20"}},
	}
	seedNextActions(t, repo, base, actions)

	// A later edit addresses src/auth.ts; it should drop from pending.
	seed(t, repo, models.LedgerEntry{
		Timestamp: base.Add(time.Minute),
		Command:   "ground",
		Outcome:   "grounded",
		Files:     []string{"src/auth.ts"},
	})

	st, err := Digest(repo, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingNextActions) != 1 {
		t.Fatalf("pending = %d, want 1 (auth.ts addressed)", len(st.PendingNextActions))
	}
	if got := actionFile(st.PendingNextActions[0]); got != "src/user.ts" {
		t.Fatalf("remaining pending targets %q, want src/user.ts", got)
	}
}

func TestClassifyAttemptsFailuresDecisions(t *testing.T) {
	repo := t.TempDir()
	base := time.Now().UTC().Add(-time.Hour)

	seed(t, repo, models.LedgerEntry{Timestamp: base, Query: "fetch auth context"})                                  // attempt, ok
	seed(t, repo, models.LedgerEntry{Timestamp: base.Add(time.Minute), Command: "ground", Outcome: "concerning"})    // failed
	seed(t, repo, models.LedgerEntry{Timestamp: base.Add(2 * time.Minute), Command: "test", Outcome: "test failed"}) // failed
	seed(t, repo, models.LedgerEntry{Timestamp: base.Add(3 * time.Minute), Command: CommandDecision, Notes: "go with JWT"})

	st, err := Digest(repo, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Decisions) != 1 || st.Decisions[0].Text != "go with JWT" {
		t.Fatalf("decisions = %+v, want one 'go with JWT'", st.Decisions)
	}
	if len(st.Failures) != 2 {
		t.Fatalf("failures = %d, want 2", len(st.Failures))
	}
	// attempts = the ok one + the two failed ones (decision is not an attempt).
	if len(st.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(st.Attempts))
	}
}

func TestOutcomeFailed(t *testing.T) {
	for _, ok := range []string{"", "grounded", "passed", "done"} {
		if outcomeFailed(ok) {
			t.Fatalf("%q should not be failed", ok)
		}
	}
	for _, bad := range []string{"test failed", "ERROR: boom", "concerning", "regressed"} {
		if !outcomeFailed(bad) {
			t.Fatalf("%q should be failed", bad)
		}
	}
}

// seedNextActions appends a next_actions ledger entry with an explicit
// timestamp (RecordNextActions stamps "now", which the ordering test cannot
// control).
func seedNextActions(t *testing.T, repo string, ts time.Time, actions []agentcontext.NextAction) {
	t.Helper()
	payload := mustJSON(t, actions)
	var files []string
	for _, a := range actions {
		if f := actionFile(a); f != "" {
			files = append(files, f)
		}
	}
	seed(t, repo, models.LedgerEntry{
		Timestamp: ts,
		Command:   CommandNextActions,
		Files:     files,
		Notes:     payload,
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDigestGroundingScopedToSession(t *testing.T) {
	repo := t.TempDir()
	seed(t, repo, models.LedgerEntry{Query: "warm up the session"})

	inCtx := true
	// One event for this session, one for another session, one unlabeled
	// (legacy manual append). The digest must count ours + the unlabeled one,
	// and must NOT absorb the other session's signal.
	for _, ev := range []grounding.Event{
		{SessionID: sess, Kind: grounding.KindEdit, FileInContext: &inCtx},
		{SessionID: "other-session", Kind: grounding.KindEdit, FileInContext: &inCtx},
		{SessionID: "", Kind: grounding.KindResponse, GroundedRatio: 1.0},
	} {
		if err := grounding.Append(repo, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	st, err := Digest(repo, sess)
	if err != nil {
		t.Fatal(err)
	}
	if st.Grounding.Events != 2 {
		t.Fatalf("grounding events = %d, want 2 (ours + unlabeled, not other-session's): %+v",
			st.Grounding.Events, st.Grounding)
	}
	if st.Grounding.Edits != 1 || st.Grounding.Responses != 1 {
		t.Fatalf("edits/responses = %d/%d, want 1/1", st.Grounding.Edits, st.Grounding.Responses)
	}
}
