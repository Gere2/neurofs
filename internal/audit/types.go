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

	// Human annotations. Metrics tell us how disciplined the answer was;
	// these tell us what the human was trying to do, why, and what they
	// concluded once the dust settled. All three are optional and
	// omitempty — legacy records on disk decode cleanly with zero values,
	// and audit logic ignores them entirely.
	//
	//   Title — short label ("008 ui hardening")
	//   Brief — multi-line task description / intent captured at pack time
	//   Note  — post-hoc conclusion written after inspecting the audit
	Title string `json:"title,omitempty"`
	Brief string `json:"brief,omitempty"`
	Note  string `json:"note,omitempty"`

	// Stats freezes the BundleStats the packager produced for this run so
	// "how much context did NeuroFS save" survives next to "how grounded
	// was the answer". A pointer + omitempty keeps legacy records
	// (written before this field existed) decoding cleanly with Stats==nil
	// — the UI renders such rows as "—" rather than as zero-cost runs.
	Stats *models.BundleStats `json:"stats,omitempty"`

	// ParentRecord is the basename (e.g. "1776696402-ddbb265c-abc123.json")
	// of the audit record that this run was *resumed from*. It is stamped
	// only when the user explicitly clicks "Resume" on a Journal card and
	// then runs a new pack+replay; legacy records and from-scratch runs
	// have it empty (omitempty drops it from JSON).
	//
	// The contract is intentionally narrow: a single back-pointer per
	// record, never a tree. Walking the chain forwards is the consumer's
	// job; a record never knows its children. This keeps the schema flat
	// and avoids any "branching" semantics the audit layer would have to
	// reason about.
	ParentRecord string `json:"parent_record,omitempty"`
}
