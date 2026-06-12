package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/loopstate"
	"github.com/spf13/cobra"
)

func newRecallCmd() *cobra.Command {
	var (
		repoPath string
		session  string
		jsonOut  bool
	)

	cmd := &cobra.Command{
		Use:   "recall",
		Short: "Read the session state a restarting loop needs: tried, failed, decided, next",
		Long: `Recall distills the session ledger and the grounding feed into one
"where am I" digest for an autonomous loop restarting mid-task:

  - attempts  : what was tried (task runs, edits, grounding events)
  - failures  : attempts whose outcome regressed or was flagged
  - decisions : choices logged with 'memory log --command decide'
  - next      : the NextActions NeuroFS suggested that are still pending
  - grounding : the rolling verification signal from 'neurofs ground'

It reads only artefacts already on disk (the SQLite ledger and
audit/grounding.jsonl), so every line is inspectable with 'neurofs memory list'
and 'neurofs ground --feed'. Defaults to the active session.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("recall: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("recall: config: %w", err)
			}
			state, err := loopstate.Digest(cfg.RepoRoot, session)
			if err != nil {
				return fmt.Errorf("recall: %w", err)
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
			}
			printRecall(cmd.OutOrStdout(), state)
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&session, "session", "", "Session to recall (defaults to the active session)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print machine-readable JSON")
	return cmd
}

func printRecall(w io.Writer, s loopstate.State) {
	fmt.Fprintf(w, "NeuroFS — session recall\n  %s\n", s.Summary)

	if len(s.PendingNextActions) > 0 {
		fmt.Fprintf(w, "\n  next actions (pending):\n")
		for _, a := range s.PendingNextActions {
			reason := a.Reason
			if reason != "" {
				reason = " — " + reason
			}
			fmt.Fprintf(w, "    → %s %s%s\n", a.Tool, compactInput(a.Input), reason)
		}
	}

	if len(s.Failures) > 0 {
		fmt.Fprintf(w, "\n  failures / flagged:\n")
		for _, f := range lastNAttempts(s.Failures, 5) {
			fmt.Fprintf(w, "    ✗ [%s] %s %s\n", f.When.Local().Format("01-02 15:04"),
				firstNonEmptyStr(f.Query, f.Command), f.Outcome)
			if len(f.Files) > 0 {
				fmt.Fprintf(w, "        files: %s\n", strings.Join(f.Files, ", "))
			}
		}
	}

	if len(s.Decisions) > 0 {
		fmt.Fprintf(w, "\n  decisions:\n")
		for _, d := range lastN(s.Decisions, 5) {
			fmt.Fprintf(w, "    • [%s] %s\n", d.When.Local().Format("01-02 15:04"), d.Text)
		}
	}

	if len(s.Attempts) > 0 {
		fmt.Fprintf(w, "\n  recent attempts:\n")
		for _, a := range lastNAttempts(s.Attempts, 5) {
			label := firstNonEmptyStr(a.Query, a.Command, "(activity)")
			mark := "·"
			if a.Failed {
				mark = "✗"
			}
			fmt.Fprintf(w, "    %s [%s] %s\n", mark, a.When.Local().Format("01-02 15:04"), label)
		}
	}

	if s.Grounding.Events == 0 && len(s.Attempts) == 0 {
		fmt.Fprintf(w, "\n  (fresh session — nothing recorded yet)\n")
	}
}

func compactInput(input map[string]any) string {
	if len(input) == 0 {
		return "{}"
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func lastN(xs []loopstate.Decision, n int) []loopstate.Decision {
	if len(xs) <= n {
		return xs
	}
	return xs[len(xs)-n:]
}

func lastNAttempts(xs []loopstate.Attempt, n int) []loopstate.Attempt {
	if len(xs) <= n {
		return xs
	}
	return xs[len(xs)-n:]
}
