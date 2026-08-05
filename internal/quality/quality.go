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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gere2/neurofs/internal/runid"
	"github.com/Gere2/neurofs/internal/safefile"
)

// Rating values are intentionally three: a binary yes/no plus an
// explicit "skip" so a user who triggered --rate by accident can
// dismiss without polluting the dataset with a false negative.
const (
	RatingYes  = "yes"
	RatingNo   = "no"
	RatingSkip = "skip"

	SourceHuman     = "human"
	SourceAgent     = "agent"
	SourceSynthetic = "synthetic"
)

// Entry is one rated prompt run. The shape is intentionally flat so
// `jq`, `awk`, or pandas can slice the file without nested traversal.
type Entry struct {
	runid.Availability
	Timestamp     time.Time `json:"ts"`
	Query         string    `json:"query"`
	Repo          string    `json:"repo"`
	BundlePath    string    `json:"bundle_path,omitempty"`
	BundleHash    string    `json:"bundle_hash,omitempty"`
	TokensUsed    int       `json:"tokens_used"`
	TokensBudget  int       `json:"tokens_budget"`
	FilesIncluded int       `json:"files_included"`
	TopPicks      []string  `json:"top_picks"`
	Reused        bool      `json:"reused"`
	Rating        string    `json:"rating"`
	Source        string    `json:"source,omitempty"`
	Comment       string    `json:"comment,omitempty"`
}

// IsHumanRating reports whether an entry is eligible for the real-use gate.
// Legacy task --rate entries did not carry a source, so they remain human by
// default. Old agent-generated entries that explicitly identify themselves as
// non-human are excluded without rewriting the append-only ledger.
func IsHumanRating(e Entry) bool {
	switch strings.ToLower(strings.TrimSpace(e.Source)) {
	case SourceHuman:
		return true
	case SourceAgent, SourceSynthetic:
		return false
	case "":
		comment := strings.ToLower(e.Comment)
		return !strings.Contains(comment, "not a human rating") &&
			!strings.Contains(comment, "agent self-assessment")
	default:
		return false
	}
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
	return AppendContext(context.Background(), repoRoot, e)
}

// AppendContext appends a rating after binding the current run attribution.
// Missing attribution is recorded explicitly; malformed/conflicting identity
// fails closed instead of silently writing a mislabeled rating.
func AppendContext(ctx context.Context, repoRoot string, e Entry) error {
	attribution, err := runid.Bind(ctx, e.Availability)
	if err != nil {
		return fmt.Errorf("quality: bind run identity: %w", err)
	}
	e.Availability = attribution
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	p := Path(repoRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("quality: mkdir: %w", err)
	}
	f, err := safefile.OpenAppend(p, 0o600)
	if err != nil {
		return fmt.Errorf("quality: open %s: %w", p, err)
	}
	enc := json.NewEncoder(f) // newline-delimited by default
	if err := enc.Encode(e); err != nil {
		_ = f.Close()
		return fmt.Errorf("quality: encode: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("quality: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("quality: close: %w", err)
	}
	return nil
}
