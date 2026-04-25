// Package quality records human ratings of task prompts to a per-repo
// JSONL file. The goal is the cheapest possible feedback loop: after
// `neurofs task --rate`, the user answers y/n with an optional one-
// line comment, and the entry lands in .neurofs/quality.jsonl with
// the query, top picks, tokens, and cache status.
//
// Once a few weeks of entries accumulate, the file is the most honest
// signal we have for whether the ranker is doing its job — far more
// useful than synthetic benchmarks against retrofit queries.
package quality

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Rating values are intentionally three: a binary yes/no plus an
// explicit "skip" so a user who triggered --rate by accident can
// dismiss without polluting the dataset with a false negative.
const (
	RatingYes  = "yes"
	RatingNo   = "no"
	RatingSkip = "skip"
)

// Entry is one rated prompt run. The shape is intentionally flat so
// `jq`, `awk`, or pandas can slice the file without nested traversal.
type Entry struct {
	Timestamp     time.Time `json:"ts"`
	Query         string    `json:"query"`
	Repo          string    `json:"repo"`
	TokensUsed    int       `json:"tokens_used"`
	TokensBudget  int       `json:"tokens_budget"`
	FilesIncluded int       `json:"files_included"`
	TopPicks      []string  `json:"top_picks"`
	Reused        bool      `json:"reused"`
	Rating        string    `json:"rating"`
	Comment       string    `json:"comment,omitempty"`
}

// Path returns the absolute location of the quality log for repoRoot.
// Callers can show this to the user so they know where the feedback
// is going — opaque file paths erode trust in local-first tools.
func Path(repoRoot string) string {
	return filepath.Join(repoRoot, ".neurofs", "quality.jsonl")
}

// Append writes one entry as a single line of JSON, creating the file
// (and the .neurofs directory) on first use. Append is the only
// supported write mode: rewriting an existing rating would silently
// reshape the historical record, which is exactly what this dataset
// must not do.
func Append(repoRoot string, e Entry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	p := Path(repoRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("quality: mkdir: %w", err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("quality: open %s: %w", p, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f) // newline-delimited by default
	if err := enc.Encode(e); err != nil {
		return fmt.Errorf("quality: encode: %w", err)
	}
	return nil
}
