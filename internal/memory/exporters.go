package memory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// TimelineExporter generates a developer activity summary timeline (formerly CLAUDE.md format).
type TimelineExporter struct{}

func (TimelineExporter) Export(sessionID string, entries []models.LedgerEntry) (string, error) {
	consolidated := consolidateEntries(entries)
	var b strings.Builder
	fmt.Fprintf(&b, "# Session Summary (NEUROFS_SESSION.md)\n")
	fmt.Fprintf(&b, "**Active Session ID**: `%s`\n\n", sessionID)

	fmt.Fprintf(&b, "## Chronological Activity\n")
	for _, e := range consolidated {
		tStr := e.Timestamp.Format("2006-01-02 15:04:05")
		var parts []string
		if e.Query != "" {
			parts = append(parts, fmt.Sprintf("Query: %q", e.Query))
		}
		if e.Command != "" {
			parts = append(parts, fmt.Sprintf("Command: `%s`", sanitizeMarkdownCode(e.Command)))
		}
		if e.Outcome != "" {
			parts = append(parts, fmt.Sprintf("Outcome: *%s*", e.Outcome))
		}
		if e.Notes != "" {
			parts = append(parts, fmt.Sprintf("(%s)", e.Notes))
		}

		if len(parts) > 0 {
			fmt.Fprintf(&b, "- **%s**: %s\n", tStr, strings.Join(parts, " | "))
		}
	}
	fmt.Fprintf(&b, "\n")

	// Collect unique files
	uniqueFiles := make(map[string]bool)
	for _, e := range consolidated {
		for _, f := range e.Files {
			uniqueFiles[f] = true
		}
	}
	if len(uniqueFiles) > 0 {
		fmt.Fprintf(&b, "## Unique Files Accessed/Modified\n")
		var sortedFiles []string
		for f := range uniqueFiles {
			sortedFiles = append(sortedFiles, f)
		}
		sort.Strings(sortedFiles)
		for _, f := range sortedFiles {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		fmt.Fprintf(&b, "\n")
	}

	// Collect commands
	var executedCommands []string
	for _, e := range consolidated {
		if e.Command != "" {
			outcomeStr := ""
			if e.Outcome != "" {
				outcomeStr = fmt.Sprintf(" (Outcome: *%s*)", e.Outcome)
			}
			executedCommands = append(executedCommands, fmt.Sprintf("`%s`%s", sanitizeMarkdownCode(e.Command), outcomeStr))
		}
	}
	if len(executedCommands) > 0 {
		fmt.Fprintf(&b, "## Commands Executed\n")
		for _, cmd := range executedCommands {
			fmt.Fprintf(&b, "- %s\n", cmd)
		}
	}

	return b.String(), nil
}

// AgentsExporter generates handoff context details for external LLM subagents.
type AgentsExporter struct{}

func (AgentsExporter) Export(sessionID string, entries []models.LedgerEntry) (string, error) {
	consolidated := consolidateEntries(entries)
	var b strings.Builder
	fmt.Fprintf(&b, "# Agent Handoff Context (AGENTS.md)\n")
	fmt.Fprintf(&b, "**Active Session ID**: `%s`\n\n", sessionID)

	// Summarize latest status cleanly
	var lastQuery string
	var lastOutcome string
	var lastNotes string
	for i := len(consolidated) - 1; i >= 0; i-- {
		e := consolidated[i]
		if lastQuery == "" && e.Query != "" {
			lastQuery = e.Query
		}
		if lastOutcome == "" && e.Outcome != "" {
			lastOutcome = e.Outcome
		}
		if lastNotes == "" && e.Notes != "" {
			lastNotes = e.Notes
		}
	}

	fmt.Fprintf(&b, "## Latest Session State\n")
	if lastQuery != "" {
		fmt.Fprintf(&b, "- **Last Query**: %q\n", lastQuery)
	}
	if lastOutcome != "" {
		fmt.Fprintf(&b, "- **Last Outcome**: *%s*\n", lastOutcome)
	}
	if lastNotes != "" {
		fmt.Fprintf(&b, "- **Last Notes**: %s\n", lastNotes)
	}
	fmt.Fprintf(&b, "\n")

	// Collect unique files
	uniqueFiles := make(map[string]bool)
	for _, e := range consolidated {
		for _, f := range e.Files {
			uniqueFiles[f] = true
		}
	}
	if len(uniqueFiles) > 0 {
		fmt.Fprintf(&b, "## Working Set Files\n")
		var sortedFiles []string
		for f := range uniqueFiles {
			sortedFiles = append(sortedFiles, f)
		}
		sort.Strings(sortedFiles)
		for _, f := range sortedFiles {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		fmt.Fprintf(&b, "\n")
	}

	// Commands run
	var executedCommands []string
	for _, e := range consolidated {
		if e.Command != "" {
			outcomeStr := ""
			if e.Outcome != "" {
				outcomeStr = fmt.Sprintf(" → *%s*", e.Outcome)
			}
			executedCommands = append(executedCommands, fmt.Sprintf("`%s`%s", sanitizeMarkdownCode(e.Command), outcomeStr))
		}
	}
	if len(executedCommands) > 0 {
		fmt.Fprintf(&b, "## Executed Verifications & Commands\n")
		for _, cmd := range executedCommands {
			fmt.Fprintf(&b, "- %s\n", cmd)
		}
	}

	return b.String(), nil
}

// MarkdownExporter generates a chronological table of events.
type MarkdownExporter struct{}

func (MarkdownExporter) Export(sessionID string, entries []models.LedgerEntry) (string, error) {
	consolidated := consolidateEntries(entries)
	var b strings.Builder
	fmt.Fprintf(&b, "# Session Ledger Log\n")
	fmt.Fprintf(&b, "**Active Session ID**: `%s`\n\n", sessionID)

	for _, e := range consolidated {
		tStr := e.Timestamp.Format("2006-01-02 15:04:05")
		fmt.Fprintf(&b, "---\n")
		fmt.Fprintf(&b, "### 📅 %s UTC\n", tStr)
		if e.Query != "" {
			fmt.Fprintf(&b, "- **Query**: %q\n", e.Query)
		}
		if e.BundleHash != "" {
			fmt.Fprintf(&b, "- **Bundle Hash**: `%s`\n", e.BundleHash)
		}
		if len(e.Files) > 0 {
			fmt.Fprintf(&b, "- **Involved Files**:\n")
			for _, f := range e.Files {
				fmt.Fprintf(&b, "  - `%s`\n", f)
			}
		}
		if e.Command != "" {
			fmt.Fprintf(&b, "- **Command**: `%s`\n", sanitizeMarkdownCode(e.Command))
		}
		if e.Outcome != "" {
			fmt.Fprintf(&b, "- **Outcome**: *%s*\n", e.Outcome)
		}
		if e.Notes != "" {
			fmt.Fprintf(&b, "- **Notes**: %s\n", e.Notes)
		}
	}
	return b.String(), nil
}

func sanitizeMarkdownCode(s string) string {
	return strings.ReplaceAll(s, "`", "")
}

func consolidateEntries(entries []models.LedgerEntry) []models.LedgerEntry {
	var consolidated []models.LedgerEntry
	for _, e := range entries {
		if len(consolidated) > 0 {
			last := &consolidated[len(consolidated)-1]
			// Consolidate consecutive duplicate logs having identical Query, Command, and Outcome
			if last.Query == e.Query && last.Command == e.Command && last.Outcome == e.Outcome {
				if last.Notes != e.Notes && e.Notes != "" {
					if last.Notes == "" {
						last.Notes = e.Notes
					} else if !strings.Contains(last.Notes, e.Notes) {
						last.Notes = fmt.Sprintf("%s; %s", last.Notes, e.Notes)
					}
				}
				for _, f := range e.Files {
					found := false
					for _, lf := range last.Files {
						if lf == f {
							found = true
							break
						}
					}
					if !found {
						last.Files = append(last.Files, f)
					}
				}
				continue
			}
		}
		// Copy slice to avoid mutating slice backing arrays
		filesCopy := make([]string, len(e.Files))
		copy(filesCopy, e.Files)
		e.Files = filesCopy
		consolidated = append(consolidated, e)
	}
	return consolidated
}
