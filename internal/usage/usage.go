// Package usage records what retrieval actually delivered (usage.jsonl)
// and what the consumer said about it afterwards (feedback.jsonl). Together
// the two ledgers are the raw signal the learn loop feeds on: usage says
// "for this query, these chunks were served"; feedback says "these of them
// mattered, these identifiers were missing". `neurofs learn promote` joins
// them into G3-style fixtures and `neurofs learn tune` optimizes ranking
// weights against those fixtures.
//
// Both files are append-only JSONL under .neurofs/, same discipline as
// quality.jsonl: rewriting history would reshape the training signal.
package usage

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Hit is one delivered chunk, trimmed to what learning needs: identity and
// why it ranked. Snippets are deliberately not stored — the ledger must stay
// cheap to append on every MCP call and safe to keep around.
type Hit struct {
	Path      string   `json:"path"`
	Symbol    string   `json:"symbol,omitempty"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Score     float64  `json:"score"`
	Reasons   []string `json:"reasons,omitempty"`
}

// Entry is one retrieval served to a consumer.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	ID        string    `json:"id"`
	Source    string    `json:"source"` // "mcp" or "cli"
	Tool      string    `json:"tool"`   // e.g. "neurofs_search", "neurofs_context"
	Query     string    `json:"query"`
	Mode      string    `json:"mode,omitempty"` // search mode or context route
	Hits      []Hit     `json:"hits,omitempty"`
	Tokens    int       `json:"tokens"` // token estimate of the delivered context
}

// Feedback ratings mirror quality's yes/no plus "partial" for retrievals
// that helped but missed something — the most informative case, since it
// usually carries MissingFacts.
const (
	RatingYes     = "yes"
	RatingNo      = "no"
	RatingPartial = "partial"
)

// Feedback is one post-task judgement about a served retrieval.
type Feedback struct {
	Timestamp     time.Time `json:"ts"`
	UsageID       string    `json:"usage_id,omitempty"`
	Query         string    `json:"query"`
	Rating        string    `json:"rating"`
	UsefulPaths   []string  `json:"useful_paths,omitempty"`
	UsefulSymbols []string  `json:"useful_symbols,omitempty"`
	MissingFacts  []string  `json:"missing_facts,omitempty"`
	Comment       string    `json:"comment,omitempty"`
}

// Path returns the usage ledger location for repoRoot.
func Path(repoRoot string) string {
	return filepath.Join(repoRoot, ".neurofs", "usage.jsonl")
}

// FeedbackPath returns the feedback ledger location for repoRoot.
func FeedbackPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".neurofs", "feedback.jsonl")
}

// Append writes one usage entry, filling Timestamp and ID when empty.
func Append(repoRoot string, e Entry) (Entry, error) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.ID == "" {
		e.ID = entryID(e.Timestamp, e.Query)
	}
	if err := appendJSONL(Path(repoRoot), e); err != nil {
		return e, err
	}
	return e, nil
}

// AppendFeedback writes one feedback entry, filling Timestamp when empty.
func AppendFeedback(repoRoot string, f Feedback) error {
	if f.Timestamp.IsZero() {
		f.Timestamp = time.Now().UTC()
	}
	return appendJSONL(FeedbackPath(repoRoot), f)
}

// Load reads the usage ledger, oldest first. Unparseable lines are skipped:
// a corrupted line must not invalidate the rest of the history.
func Load(repoRoot string) ([]Entry, error) {
	return loadJSONL[Entry](Path(repoRoot))
}

// LoadFeedback reads the feedback ledger, oldest first.
func LoadFeedback(repoRoot string) ([]Feedback, error) {
	return loadJSONL[Feedback](FeedbackPath(repoRoot))
}

// MatchEntry finds the usage entry a feedback refers to: the most recent
// entry whose query equals q (case-insensitive), or simply the most recent
// entry when q is empty. Feedback normally arrives right after the
// retrieval it judges, so recency is the right tiebreak.
func MatchEntry(entries []Entry, q string) (Entry, bool) {
	q = strings.ToLower(strings.TrimSpace(q))
	for i := len(entries) - 1; i >= 0; i-- {
		if q == "" || strings.ToLower(strings.TrimSpace(entries[i].Query)) == q {
			return entries[i], true
		}
	}
	return Entry{}, false
}

func entryID(ts time.Time, query string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", ts.UnixNano(), query)))
	return hex.EncodeToString(sum[:])[:12]
}

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("usage: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("usage: open %s: %w", path, err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(v); err != nil {
		return fmt.Errorf("usage: encode: %w", err)
	}
	return nil
}

func loadJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("usage: open %s: %w", path, err)
	}
	defer f.Close()

	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("usage: scan %s: %w", path, err)
	}
	return out, nil
}
