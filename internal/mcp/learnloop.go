package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/neuromfs/neuromfs/internal/usage"
)

// This file is the MCP half of the learn loop: every retrieval served over
// MCP lands in .neurofs/usage.jsonl, and the neurofs_feedback tool lets the
// agent close the loop by reporting what actually helped. `neurofs learn`
// consumes both ledgers.

const feedbackInputSchema = `{
  "type": "object",
  "properties": {
    "rating":         { "type": "string", "enum": ["yes", "no", "partial"], "description": "Did the retrieved context serve the task? partial = helped but something was missing." },
    "query":          { "type": "string", "description": "The query being rated. Default: the most recent logged retrieval." },
    "useful_paths":   { "type": "array", "items": { "type": "string" }, "description": "Repo-relative paths that were actually useful." },
    "useful_symbols": { "type": "array", "items": { "type": "string" }, "description": "Identifiers (functions, types, methods) that were actually useful." },
    "missing":        { "type": "array", "items": { "type": "string" }, "description": "Identifiers or files that SHOULD have been retrieved but were not." },
    "comment":        { "type": "string", "description": "Optional one-line note." },
    "repo":           { "type": "string", "description": "Absolute path to repo. Default: cwd." }
  },
  "required": ["rating"]
}`

type feedbackArgs struct {
	Rating        string   `json:"rating"`
	Query         string   `json:"query"`
	UsefulPaths   []string `json:"useful_paths"`
	UsefulSymbols []string `json:"useful_symbols"`
	Missing       []string `json:"missing"`
	Comment       string   `json:"comment"`
	Repo          string   `json:"repo"`
}

func runFeedbackTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args feedbackArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	rating := strings.ToLower(strings.TrimSpace(args.Rating))
	switch rating {
	case usage.RatingYes, usage.RatingNo, usage.RatingPartial:
	default:
		return errResult(`rating must be "yes", "no", or "partial"`)
	}

	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	entries, err := usage.Load(repo)
	if err != nil {
		return errResult(err.Error())
	}
	fb := usage.Feedback{
		Query:         strings.TrimSpace(args.Query),
		Rating:        rating,
		UsefulPaths:   trimNonEmpty(args.UsefulPaths),
		UsefulSymbols: trimNonEmpty(args.UsefulSymbols),
		MissingFacts:  trimNonEmpty(args.Missing),
		Comment:       strings.TrimSpace(args.Comment),
	}
	if matched, ok := usage.MatchEntry(entries, fb.Query); ok {
		fb.UsageID = matched.ID
		if fb.Query == "" {
			fb.Query = matched.Query
		}
	}
	if fb.Query == "" {
		return errResult("no query given and no logged retrieval to attach the feedback to")
	}
	if err := usage.AppendFeedback(repo, fb); err != nil {
		return errResult(err.Error())
	}

	feedbacks, _ := usage.LoadFeedback(repo)
	return jsonTextResult(map[string]any{
		"recorded":       true,
		"query":          fb.Query,
		"usage_id":       fb.UsageID,
		"feedback_count": len(feedbacks),
		"next":           "run `neurofs learn promote` to fold feedback into fixtures, then `neurofs learn tune` to improve ranking weights",
	})
}

// logSearchUsage appends one usage entry for a served retrieval. Logging is
// best-effort by design: a full disk or read-only checkout must never fail
// the retrieval the caller actually asked for.
func logSearchUsage(repo, tool, query, mode string, hits []SearchResultHit, bundleTokens int) {
	if strings.TrimSpace(query) == "" {
		return
	}
	entry := usage.Entry{
		Source: "mcp",
		Tool:   tool,
		Query:  query,
		Mode:   mode,
		Tokens: bundleTokens,
	}
	for _, h := range hits {
		entry.Tokens += h.TokenEstimate
		entry.Hits = append(entry.Hits, usage.Hit{
			Path:      h.Path,
			Symbol:    h.Symbol,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Score:     h.Score,
			Reasons:   h.Reasons,
		})
	}
	_, _ = usage.Append(repo, entry)
}

func trimNonEmpty(items []string) []string {
	var out []string
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
