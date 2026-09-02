package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Gere2/neurofs/internal/tokenbudget"
	"github.com/Gere2/neurofs/internal/usage"
)

// This file is the MCP half of the learn loop: every retrieval served over
// MCP lands in .neurofs/usage.jsonl, and the neurofs_feedback tool lets the
// agent close the loop by reporting what actually helped. `neurofs learn`
// consumes both ledgers.

const feedbackInputSchema = `{
  "type": "object",
  "properties": {
    "rating":         { "type": "string", "enum": ["yes", "no", "partial"], "description": "Did the retrieved context serve the task? partial = helped but something was missing." },
    "retrieval_id":   { "type": "string", "description": "Exact retrieval_id returned by neurofs_context, neurofs_search, or neurofs_expand. Preferred over query matching." },
    "usage_id":       { "type": "string", "description": "Deprecated alias for retrieval_id." },
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
	RetrievalID   string   `json:"retrieval_id"`
	UsageID       string   `json:"usage_id"`
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
		ActorKind:     "agent",
	}
	retrievalID := strings.TrimSpace(args.RetrievalID)
	if retrievalID == "" {
		retrievalID = strings.TrimSpace(args.UsageID)
	}
	if retrievalID != "" {
		matched, ok := usage.MatchEntryByID(entries, retrievalID)
		if !ok {
			return errResult(fmt.Sprintf("retrieval_id %q was not found in the usage ledger", retrievalID))
		}
		if fb.Query != "" && !strings.EqualFold(fb.Query, strings.TrimSpace(matched.Query)) {
			return errResult("query does not match the retrieval identified by retrieval_id")
		}
		fb.UsageID = matched.ID
		fb.Query = matched.Query
	} else if matched, ok := usage.MatchEntry(entries, fb.Query); ok {
		fb.UsageID = matched.ID
		if fb.Query == "" {
			fb.Query = matched.Query
		}
	}
	if fb.Query == "" {
		return errResult("no query given and no logged retrieval to attach the feedback to")
	}
	if err := usage.AppendFeedbackContext(ctx, repo, fb); err != nil {
		return errResult(err.Error())
	}

	feedbacks, _ := usage.LoadFeedback(repo)
	return jsonTextResult(map[string]any{
		"recorded":       true,
		"query":          fb.Query,
		"retrieval_id":   fb.UsageID,
		"usage_id":       fb.UsageID,
		"feedback_count": len(feedbacks),
		"next":           "run `neurofs learn promote` to fold feedback into fixtures, then `neurofs learn tune` to improve ranking weights",
	})
}

// logSearchUsage appends one usage entry for a served retrieval. Payload
// metrics describe the serialized response the consumer actually received;
// HitTokens preserves the older estimate of the ranked context inside it.
// Logging is best-effort: a full disk or read-only checkout must never fail
// the retrieval the caller actually asked for.
func logSearchUsage(ctx context.Context, repo string, entry usage.Entry, hits []SearchResultHit, bundleTokens int, payload []byte) {
	if strings.TrimSpace(entry.Query) == "" {
		return
	}
	entry.HitTokens = bundleTokens
	for _, h := range hits {
		entry.HitTokens += h.TokenEstimate
		entry.Hits = append(entry.Hits, usage.Hit{
			Path:      h.Path,
			Symbol:    h.Symbol,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Score:     h.Score,
			Reasons:   h.Reasons,
		})
	}
	entry.PayloadBytes = len(payload)
	entry.PayloadTokens = tokenbudget.EstimateTokens(string(payload))
	entry.Tokens = entry.PayloadTokens
	if elapsed := time.Since(entry.Timestamp); elapsed > 0 {
		entry.LatencyMS = int64((elapsed + time.Millisecond - 1) / time.Millisecond)
	}
	_, _ = usage.AppendContext(ctx, repo, entry)
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
