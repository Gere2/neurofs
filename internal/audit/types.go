// Package audit closes the governance loop on NeuroFS: it takes a bundle
// produced by the packager and a model response, and produces a replayable
// record of how disciplined that response was. No network calls live here —
// the Model interface is injected so tests (and CI) can run the full audit
// pipeline against a deterministic stub without spending LLM tokens.
package audit

import (
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

// Citation is a single path (optionally with a line number) the model
// referenced in its response. Valid is true when the cited file appears in
// the bundle — otherwise Reason explains why we rejected it.
type Citation struct {
	Raw     string `json:"raw"`      // the literal span we extracted (e.g. "src/auth.ts:42")
	RelPath string `json:"rel_path"` // normalised path portion
	Line    int    `json:"line,omitempty"`
	Valid   bool   `json:"valid"`
	Reason  string `json:"reason,omitempty"` // populated on !Valid
}

// DriftReport tallies everything the response mentioned that was not backed
// by the bundle. We split drift into three buckets so the user can see
// which axis the model slipped on:
//
//   - UnknownPaths   : file-like references (foo/bar.ts, utils.py)
//   - UnknownAPIs    : dotted names (jwt.sign, os.path.join)
//   - UnknownSymbols : plain identifiers (AuthController, session_store)
//
// Rate is unknown / (unknown + known) over the joined set — a cheap
// hallucination signal on the output. Narrative tokens like "This" or
// "Overall" never reach these lists; see drift.go for the classifier.
type DriftReport struct {
	UnknownPaths   []string `json:"unknown_paths,omitempty"`
	UnknownAPIs    []string `json:"unknown_apis,omitempty"`
	UnknownSymbols []string `json:"unknown_symbols,omitempty"`
	KnownCount     int      `json:"known_count"`
	UnknownCount   int      `json:"unknown_count"`
	Rate           float64  `json:"rate"`
}

// AuditFragment is a frozen snapshot of what the bundle carried at audit
// time. We copy instead of holding the Bundle pointer so the record is
// self-contained on disk: a diff of index.db later does not invalidate it.
type AuditFragment struct {
	RelPath        string                `json:"rel_path"`
	Lang           models.Lang           `json:"lang"`
	Representation models.Representation `json:"representation"`
	Tokens         int                   `json:"tokens"`
	// Content is intentionally kept — governance replay needs to re-verify
	// citations against the exact text the model saw.
	Content string `json:"content"`
}

// AuditRecord is one full governance observation. It is the persistence unit
// and the thing CI can diff across runs.
//
// Mode is optional (omitempty) and retrofits the notion of "how was this
// bundle meant to be used" onto the record: strategy / build / review today,
// open-ended tomorrow. Records generated before the field existed unmarshal
// cleanly with Mode == "" — no migration needed.
type AuditRecord struct {
	Question   string          `json:"question"`
	Model      string          `json:"model"`      // free-form id, e.g. "stub", "claude-sonnet-4-6"
	Mode       string          `json:"mode,omitempty"` // strategy | build | review | ""
	Timestamp  time.Time       `json:"timestamp"`
	BundleHash string          `json:"bundle_hash"`
	Fragments  []AuditFragment `json:"fragments"`
	Response   string          `json:"response"`

	Citations     []Citation  `json:"citations"`
	Drift         DriftReport `json:"drift"`
	GroundedRatio float64     `json:"grounded_ratio"`

	// Optional fact-recall scoring. Populated only when ExpectsFacts is
	// non-empty on the benchmark question.
	ExpectsFacts []string `json:"expects_facts,omitempty"`
	FactsHit     []string `json:"facts_hit,omitempty"`
	AnswerRecall float64  `json:"answer_recall,omitempty"`
}
