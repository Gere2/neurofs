package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/grounding"
	"github.com/Gere2/neurofs/internal/memory"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/safefile"
	"github.com/spf13/cobra"
)

const (
	maxHookEventBytes      int64 = 4 << 20
	maxGroundBundleBytes   int64 = 64 << 20
	maxTranscriptLineBytes       = 4 << 20
)

// hookEvent is the subset of a Claude Code hook payload neurofs ground reads
// from stdin. Unknown fields are ignored, so it tolerates payload evolution.
type hookEvent struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

type toolInput struct {
	FilePath  string `json:"file_path"`
	Content   string `json:"content"`    // Write
	NewString string `json:"new_string"` // Edit
	Edits     []struct {
		NewString string `json:"new_string"`
	} `json:"edits"` // MultiEdit
}

func newGroundCmd() *cobra.Command {
	var (
		repoPath   string
		bundlePath string
		feed       bool
		jsonOut    bool
		printHook  bool
		limit      int
	)

	cmd := &cobra.Command{
		Use:   "ground",
		Short: "Continuously ground agent actions against the context they were given",
		Long: `Ground turns audit grounding into a continuous signal for autonomous loops.

Wire it as a Claude Code hook and it records, after every agent action, whether
that action was anchored in the context NeuroFS provided — accumulating an
append-only ledger (audit/grounding.jsonl) a supervisor reads at a glance
instead of reading every diff.

  # Install (prints the .claude/settings.json snippet to paste)
  neurofs ground --print-hook

  # Hook invocation (reads the event JSON on stdin) — what Claude Code runs
  neurofs ground

  # Supervisor feed: the rolling aggregate
  neurofs ground --feed
  neurofs ground --feed --json

For a PostToolUse Edit/Write event it scores whether the edited file was in the
agent's context and how much of the added code is anchored there. For a Stop
event it pulls the agent's final message from the transcript and runs the same
citation+drift grounding as 'audit replay' — automated, not pasted.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printHook {
				return printHookSnippet(cmd.OutOrStdout())
			}
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("ground: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("ground: config: %w", err)
			}

			if feed {
				return printGroundFeed(cmd.OutOrStdout(), cfg.RepoRoot, jsonOut, limit)
			}

			// Hook mode: read the event JSON from stdin.
			data, err := readGroundInput(cmd.InOrStdin(), maxHookEventBytes)
			if err != nil {
				return fmt.Errorf("ground: read hook event: %w", err)
			}
			if len(strings.TrimSpace(string(data))) == 0 {
				return fmt.Errorf("ground: no hook event on stdin (use --feed to read the ledger, --print-hook to install)")
			}
			var ev hookEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return fmt.Errorf("ground: parse hook event: %w", err)
			}

			bundle, bundleErr := resolveContextBundle(cfg.RepoRoot, bundlePath)
			if bundleErr != nil {
				// No prior task bundle is a legitimate first-run state. Unsafe,
				// oversized, corrupt, or explicitly requested bundles fail
				// closed instead of silently recording an ungrounded event.
				if bundlePath != "" || !isMissingGroundBundle(bundleErr) {
					return fmt.Errorf("ground: load bundle: %w", bundleErr)
				}
				bundle = models.Bundle{}
			}

			event, recorded, err := buildGroundingEvent(cfg.RepoRoot, ev, bundle)
			if err != nil {
				return fmt.Errorf("ground: build event: %w", err)
			}
			if !recorded {
				// Nothing actionable in this event (e.g. a non-edit tool); succeed quietly.
				return nil
			}
			if ev.SessionID == "" {
				event.SessionID = memory.GetSessionID(cfg.RepoRoot)
			} else {
				event.SessionID = ev.SessionID
			}
			if err := grounding.Append(cfg.RepoRoot, event); err != nil {
				return fmt.Errorf("ground: append: %w", err)
			}
			// Also drop a ledger breadcrumb so iteration memory sees grounding.
			_ = memory.AppendEntry(cfg.RepoRoot, models.LedgerEntry{
				SessionID: event.SessionID,
				Command:   "ground",
				Files:     event.Files,
				Outcome:   groundOutcome(event),
				Notes:     event.Note,
			})

			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "neurofs ground: %s\n", oneLineEvent(event)); err != nil {
				return fmt.Errorf("ground: write event summary: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to cwd, or the hook's cwd)")
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "Context bundle JSON to ground against (default: newest task bundle)")
	cmd.Flags().BoolVar(&feed, "feed", false, "Print the rolling grounding aggregate for a supervisor")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "With --feed, print machine-readable JSON")
	cmd.Flags().BoolVar(&printHook, "print-hook", false, "Print the Claude Code settings.json hook snippet and exit")
	cmd.Flags().IntVar(&limit, "limit", 10, "With --feed, how many recent concerning events to list")
	return cmd
}

// buildGroundingEvent maps a hook event to a grounding event. The bool reports
// whether the event was actionable (and should be recorded).
func buildGroundingEvent(repoRoot string, ev hookEvent, bundle models.Bundle) (grounding.Event, bool, error) {
	origin := strings.TrimSpace(ev.HookEventName)
	switch strings.ToLower(ev.ToolName) {
	case "edit", "write", "multiedit":
		var ti toolInput
		_ = json.Unmarshal(ev.ToolInput, &ti)
		added := ti.NewString
		if ti.Content != "" {
			added = ti.Content
		}
		for _, e := range ti.Edits {
			added += "\n" + e.NewString
		}
		rel := toRepoRel(repoRoot, ev.CWD, ti.FilePath)
		event := grounding.ScoreEdit(bundle, rel, added)
		event.Origin = joinOrigin(origin, ev.ToolName)
		return event, true, nil
	}

	// Stop / SubagentStop / no tool: ground the agent's final message.
	if strings.EqualFold(origin, "Stop") || strings.EqualFold(origin, "SubagentStop") || ev.ToolName == "" {
		resp, err := lastAssistantMessage(ev.TranscriptPath)
		if err != nil {
			return grounding.Event{}, false, err
		}
		if strings.TrimSpace(resp) == "" {
			return grounding.Event{}, false, nil
		}
		event := grounding.ScoreResponse(bundle, resp)
		event.Origin = origin
		if event.Origin == "" {
			event.Origin = "Stop"
		}
		return event, true, nil
	}
	return grounding.Event{}, false, nil
}

// resolveContextBundle loads the bundle to ground against: an explicit path, or
// the newest *.bundle.json in the task cache (the context NeuroFS most recently
// produced for this repo).
func resolveContextBundle(repoRoot, explicit string) (models.Bundle, error) {
	if explicit != "" {
		return readBundleFile(explicit)
	}
	taskDir := filepath.Join(repoRoot, ".neurofs", "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return models.Bundle{}, err
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bundle.json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newest = filepath.Join(taskDir, e.Name())
		}
	}
	if newest == "" {
		return models.Bundle{}, fmt.Errorf("no task bundle found in %s", taskDir)
	}
	return readBundleFile(newest)
}

func readBundleFile(path string) (models.Bundle, error) {
	data, _, err := fsutil.ReadRegularFileBounded(path, maxGroundBundleBytes)
	if err != nil {
		return models.Bundle{}, err
	}
	var b models.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return models.Bundle{}, err
	}
	return b, nil
}

// lastAssistantMessage extracts the final assistant text from a Claude Code
// transcript (JSONL). Best-effort: an unreadable or unfamiliar transcript
// yields "" and the caller skips recording rather than failing the hook.
func lastAssistantMessage(transcriptPath string) (last string, retErr error) {
	if transcriptPath == "" {
		return "", nil
	}
	f, err := safefile.OpenRead(transcriptPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			last = ""
			retErr = errors.Join(retErr, fmt.Errorf("close transcript: %w", err))
		}
	}()

	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type line struct {
		Type    string `json:"type"`
		Message struct {
			Role    string         `json:"role"`
			Content []contentBlock `json:"content"`
		} `json:"message"`
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxTranscriptLineBytes)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			continue
		}
		if l.Type != "assistant" && l.Message.Role != "assistant" {
			continue
		}
		var sb strings.Builder
		for _, c := range l.Message.Content {
			if c.Type == "text" || c.Type == "" {
				sb.WriteString(c.Text)
				sb.WriteByte('\n')
			}
		}
		if txt := strings.TrimSpace(sb.String()); txt != "" {
			last = txt
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scan transcript (line limit %d bytes): %w", maxTranscriptLineBytes, err)
	}
	return last, nil
}

func readGroundInput(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("input exceeds %d-byte limit", maxBytes)
	}
	return data, nil
}

func isMissingGroundBundle(err error) bool {
	return os.IsNotExist(err) || strings.Contains(err.Error(), "no task bundle found")
}

func printGroundFeed(w io.Writer, repoRoot string, jsonOut bool, limit int) error {
	events, err := grounding.Read(repoRoot)
	if err != nil {
		return fmt.Errorf("ground: read ledger: %w", err)
	}
	agg := grounding.Summarize(events)

	if jsonOut {
		return json.NewEncoder(w).Encode(struct {
			Aggregate grounding.Aggregate `json:"aggregate"`
			Recent    []grounding.Event   `json:"recent_concerning"`
		}{agg, recentConcerning(events, limit)})
	}

	report := newReportWriter(w)
	report.printf("NeuroFS — grounding feed (%s)\n", grounding.Path(repoRoot))
	if agg.Events == 0 {
		report.printf("  no grounding events yet — wire the hook with `neurofs ground --print-hook`\n")
		return report.err
	}
	report.printf("  events       : %d\n", agg.Events)
	if agg.Edits > 0 {
		report.printf("  edits        : %d, %.0f%% in provided context (mean added-code drift %.0f%%)\n",
			agg.Edits, agg.EditCoverage*100, agg.MeanEditDrift*100)
	}
	if agg.Responses > 0 {
		report.printf("  responses    : %d, mean grounded %.0f%%, mean drift %.0f%%\n",
			agg.Responses, agg.MeanGroundedResp*100, agg.MeanRespDrift*100)
	}
	report.printf("  concerning   : %d (%.0f%% of events)\n", agg.Concerning, pctOf(agg.Concerning, agg.Events))

	recent := recentConcerning(events, limit)
	if len(recent) > 0 {
		report.printf("\n  recent concerning:\n")
		for _, e := range recent {
			report.printf("    [%s] %s\n", e.Timestamp.Local().Format("01-02 15:04"), oneLineEvent(e))
		}
	}
	return report.err
}

func recentConcerning(events []grounding.Event, limit int) []grounding.Event {
	if limit <= 0 {
		limit = 10
	}
	var out []grounding.Event
	for i := len(events) - 1; i >= 0 && len(out) < limit; i-- {
		if !events[i].Grounded() {
			out = append(out, events[i])
		}
	}
	return out
}

func oneLineEvent(e grounding.Event) string {
	mark := "✓"
	if !e.Grounded() {
		mark = "·"
	}
	files := strings.Join(e.Files, ",")
	switch e.Kind {
	case grounding.KindEdit:
		s := fmt.Sprintf("%s edit %s", mark, files)
		if e.Note != "" {
			s += " — " + e.Note
		}
		return s
	case grounding.KindResponse:
		return fmt.Sprintf("%s response grounded %.0f%% drift %.0f%%", mark, e.GroundedRatio*100, e.DriftRate*100)
	default:
		return mark + " " + e.Kind
	}
}

func groundOutcome(e grounding.Event) string {
	if e.Grounded() {
		return "grounded"
	}
	return "concerning"
}

func joinOrigin(event, tool string) string {
	event = strings.TrimSpace(event)
	tool = strings.TrimSpace(tool)
	switch {
	case event == "" && tool == "":
		return "hook"
	case event == "":
		return tool
	case tool == "":
		return event
	default:
		return event + ":" + tool
	}
}

// toRepoRel converts a hook's file_path (often absolute) to a repo-relative
// slash path so it can match bundle fragment paths.
func toRepoRel(repoRoot, cwd, filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	abs := filePath
	if !filepath.IsAbs(abs) {
		base := cwd
		if base == "" {
			base = repoRoot
		}
		abs = filepath.Join(base, filePath)
	}
	if rel, err := filepath.Rel(repoRoot, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filePath)
}

func pctOf(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func printHookSnippet(w io.Writer) error {
	snippet := `Add to .claude/settings.json (project) so the loop grounds itself:

{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [{ "type": "command", "command": "neurofs ground" }]
      }
    ],
    "Stop": [
      { "hooks": [{ "type": "command", "command": "neurofs ground" }] }
    ]
  }
}

Then watch the rolling signal with: neurofs ground --feed
`
	_, err := io.WriteString(w, snippet)
	return err
}
