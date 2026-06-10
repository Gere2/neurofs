// Package loopstate gives an autonomous loop a single "where am I" read on
// restart. It distills the session ledger (what was tried, what failed, what
// was decided) and the grounding feed (the verification signal) into one
// structured State, and surfaces the NextActions agentcontext generated so a
// restarting loop can pick up where it left off instead of relearning.
//
// Nothing here is a black box: every item carries its origin (the ledger
// command), timestamp, files, and — for next actions — the reason the action
// was suggested. State is derived purely from artefacts already on disk
// (the SQLite ledger + audit/grounding.jsonl), so it is reproducible and
// inspectable with `neurofs memory list` and `neurofs ground --feed`.
package loopstate

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/agentcontext"
	"github.com/neuromfs/neuromfs/internal/grounding"
	"github.com/neuromfs/neuromfs/internal/memory"
	"github.com/neuromfs/neuromfs/internal/models"
)

// Ledger command vocabulary loopstate writes and recognises. Plain task runs
// and grounding events use their own commands ("", "ground"); these two are
// the loop-state-specific ones.
const (
	CommandNextActions = "next_actions"
	CommandDecision    = "decide"
)

// Attempt is one thing the loop tried, derived from a ledger entry.
type Attempt struct {
	When    time.Time `json:"when"`
	Query   string    `json:"query,omitempty"`
	Command string    `json:"command,omitempty"`
	Files   []string  `json:"files,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Note    string    `json:"note,omitempty"`
	Failed  bool      `json:"failed"`
}

// Decision is an explicit decision the loop or a human recorded.
type Decision struct {
	When  time.Time `json:"when"`
	Text  string    `json:"text"`
	Files []string  `json:"files,omitempty"`
}

// State is the digest a restarting loop reads.
type State struct {
	SessionID          string                    `json:"session_id"`
	Attempts           []Attempt                 `json:"attempts,omitempty"`
	Failures           []Attempt                 `json:"failures,omitempty"`
	Decisions          []Decision                `json:"decisions,omitempty"`
	PendingNextActions []agentcontext.NextAction `json:"pending_next_actions,omitempty"`
	Grounding          grounding.Aggregate       `json:"grounding"`
	Summary            string                    `json:"summary"`
}

// RecordNextActions persists the NextActions agentcontext produced for a query
// so a later restart can consume them. Stored as one ledger entry with command
// "next_actions" and the actions JSON in notes — origin, date, files, and
// per-action reason are all preserved.
func RecordNextActions(repoRoot, sessionID, query string, actions []agentcontext.NextAction) error {
	if len(actions) == 0 {
		return nil
	}
	payload, err := json.Marshal(actions)
	if err != nil {
		return err
	}
	files := make([]string, 0, len(actions))
	seen := map[string]bool{}
	for _, a := range actions {
		if f := actionFile(a); f != "" && !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	return memory.AppendEntry(repoRoot, models.LedgerEntry{
		SessionID: sessionID,
		Query:     query,
		Command:   CommandNextActions,
		Files:     files,
		Notes:     string(payload),
	})
}

// Digest builds the State for a session. An empty sessionID resolves to the
// active session. It reads the ledger and the grounding feed; both missing is
// a legitimate "fresh start" state, not an error.
func Digest(repoRoot, sessionID string) (State, error) {
	ctx := context.Background()
	store := memory.NewSqliteStore(repoRoot)
	if sessionID == "" {
		id, err := store.GetSessionID(ctx)
		if err != nil {
			return State{}, fmt.Errorf("loopstate: resolve session: %w", err)
		}
		sessionID = id
	}
	entries, err := store.Read(ctx, sessionID)
	if err != nil {
		return State{}, fmt.Errorf("loopstate: read ledger: %w", err)
	}

	st := State{SessionID: sessionID}

	var (
		latestActions    []agentcontext.NextAction
		actionsAt        time.Time
		filesTouchedLast = map[string]bool{} // files edited after the latest next_actions
	)

	for _, e := range entries {
		switch e.Command {
		case CommandNextActions:
			if decoded := decodeActions(e.Notes); len(decoded) > 0 {
				latestActions = decoded
				actionsAt = e.Timestamp
				filesTouchedLast = map[string]bool{} // reset; only later edits count
			}
			continue
		case CommandDecision:
			st.Decisions = append(st.Decisions, Decision{
				When:  e.Timestamp,
				Text:  firstNonEmpty(e.Notes, e.Query),
				Files: e.Files,
			})
			continue
		}
		if strings.EqualFold(e.Outcome, "decided") {
			st.Decisions = append(st.Decisions, Decision{When: e.Timestamp, Text: firstNonEmpty(e.Notes, e.Query), Files: e.Files})
			continue
		}

		// Everything else is an attempt: a task run, a grounding event, a
		// manual log. Record the files it touched so we can tell which pending
		// next actions are now addressed.
		att := Attempt{
			When:    e.Timestamp,
			Query:   e.Query,
			Command: e.Command,
			Files:   e.Files,
			Outcome: e.Outcome,
			Note:    e.Notes,
			Failed:  outcomeFailed(e.Outcome),
		}
		st.Attempts = append(st.Attempts, att)
		if att.Failed {
			st.Failures = append(st.Failures, att)
		}
		if !actionsAt.IsZero() && !e.Timestamp.Before(actionsAt) {
			for _, f := range e.Files {
				filesTouchedLast[normPath(f)] = true
			}
		}
	}

	// A next action is pending unless its target file was already touched after
	// it was suggested.
	for _, a := range latestActions {
		f := actionFile(a)
		if f != "" && filesTouchedLast[normPath(f)] {
			continue
		}
		st.PendingNextActions = append(st.PendingNextActions, a)
	}

	if events, gerr := grounding.Read(repoRoot); gerr == nil {
		st.Grounding = grounding.Summarize(events)
	}

	st.Summary = buildSummary(st)
	return st, nil
}

func buildSummary(st State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s: %d attempt(s)", st.SessionID, len(st.Attempts))
	if len(st.Failures) > 0 {
		fmt.Fprintf(&b, " (%d failed)", len(st.Failures))
	}
	fmt.Fprintf(&b, ", %d decision(s), %d pending next action(s).", len(st.Decisions), len(st.PendingNextActions))
	if st.Grounding.Events > 0 {
		fmt.Fprintf(&b, " Grounding: %d events", st.Grounding.Events)
		if st.Grounding.Edits > 0 {
			fmt.Fprintf(&b, ", %.0f%% edits in context", st.Grounding.EditCoverage*100)
		}
		if st.Grounding.Concerning > 0 {
			fmt.Fprintf(&b, ", %d concerning", st.Grounding.Concerning)
		}
		b.WriteByte('.')
	}
	return b.String()
}

// actionFile extracts the repo-relative file an action targets, if any. Inputs
// look like {"target":"path:start-end"} or {"path":"path"}.
func actionFile(a agentcontext.NextAction) string {
	if a.Input == nil {
		return ""
	}
	if v, ok := a.Input["path"].(string); ok && v != "" {
		return normPath(v)
	}
	if v, ok := a.Input["target"].(string); ok && v != "" {
		if i := strings.Index(v, ":"); i > 0 {
			v = v[:i]
		}
		return normPath(v)
	}
	return ""
}

func decodeActions(notes string) []agentcontext.NextAction {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return nil
	}
	var actions []agentcontext.NextAction
	if err := json.Unmarshal([]byte(notes), &actions); err != nil {
		return nil
	}
	return actions
}

func outcomeFailed(outcome string) bool {
	o := strings.ToLower(strings.TrimSpace(outcome))
	if o == "" {
		return false
	}
	for _, marker := range []string{"fail", "error", "concerning", "regress", "broken", "reject"} {
		if strings.Contains(o, marker) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
}
