// Package receipt defines RunReceipt v1: the canonical record that correlates
// one NeuroFS-controlled run — context bundles, policy decision, external
// attempts, usage, verification and artifacts — under a single run_id.
//
// The thesis it serves: NeuroFS prepares the context, applies the owner's
// policy and proves the result; the provider's router picks the model within
// the catalog the policy allows.
//
// The JSONL ledger (audit/run_receipts.jsonl) is the source of truth and is
// append-only history: records are never updated or deleted. Corrections and
// later judgments append run_amendment records pointing at the receipt they
// correct via corrects_receipt_id. Any SQLite view over receipts is a fully
// rebuildable index, never the source.
//
// Two invariants hold everywhere in this schema:
//
//   - Absence never means zero. Unknown cost, credits or tokens stay
//     explicitly unknown (provenance "unknown", empty quantity); quantities
//     are decimal strings, never floats.
//   - The receipt carries references, not payloads. Prompts, responses,
//     diffs and environments live as artifacts referenced by repo-relative
//     path + sha256; they are never inlined.
//
// Scope of the integrity story: the per-record content hash makes accidental
// corruption and casual in-place edits detectable without resealing. It is
// not tamper evidence — whoever can rewrite the file can delete, reorder or
// reseal records. If tamper evidence is ever required, that takes a hash
// chain plus external anchoring, deliberately out of scope for v1.
package receipt

import "time"

// SchemaVersion is the RunReceipt schema this package reads and writes.
const SchemaVersion = 1

// RecordKind discriminates the two line types of the JSONL ledger.
type RecordKind string

const (
	// KindRunReceipt is the single, final record for one run_id, written by
	// the adapter (the only writer of final receipts) when the run finishes.
	KindRunReceipt RecordKind = "run_receipt"
	// KindRunAmendment is a later correction or judgment. It never edits a
	// receipt in place: it appends, pointing at corrects_receipt_id.
	KindRunAmendment RecordKind = "run_amendment"
)

// Enforcement records how a policy restriction was actually enforced. A hard
// restriction that cannot be demonstrated is not silently trusted: the caller
// must exclude auto-routing or pin an allowed model (fail-closed).
type Enforcement string

const (
	EnforcementLocal    Enforcement = "local_enforced"    // NeuroFS enforced it before launch
	EnforcementProvider Enforcement = "provider_enforced" // provider mechanism (e.g. repo model allow-list)
	EnforcementObserved Enforcement = "observed_only"     // only verified after the fact
	EnforcementUnknown  Enforcement = "unknown"
)

// PolicyDecision is the verdict the core emits; a thin adapter executes it.
type PolicyDecision string

const (
	DecisionAllow     PolicyDecision = "allow"
	DecisionDeny      PolicyDecision = "deny"
	DecisionRecommend PolicyDecision = "recommend"
	DecisionEscalate  PolicyDecision = "escalate"
)

// Provenance labels where a figure came from. Estimated numbers are never
// presented as observed ones.
type Provenance string

const (
	ProvenanceProviderReported Provenance = "provider_reported"
	ProvenanceObserved         Provenance = "observed"
	ProvenanceEstimated        Provenance = "estimated"
	ProvenanceUnknown          Provenance = "unknown"
)

// Confidence qualifies a usage figure independently of its provenance.
type Confidence string

const (
	ConfidenceHigh    Confidence = "high"
	ConfidenceMedium  Confidence = "medium"
	ConfidenceLow     Confidence = "low"
	ConfidenceUnknown Confidence = "unknown"
)

// Classification is the v1 verdict vocabulary for a verified run.
type Classification string

const (
	// ClassificationCleanPass requires: reproducible failing baseline, one
	// single external attempt, the target verifier really executed (exit 0
	// with zero executed tests does not count), zero failures after, an
	// identical frozen verifier surface, and an untouched verifier command.
	ClassificationCleanPass Classification = "clean_pass_at_1"
	// ClassificationPassNeedsReview marks runs whose diff touched tests,
	// fixtures, snapshots, runner or configuration. Never auto-rejected,
	// never counted as a clean pass without human review.
	ClassificationPassNeedsReview Classification = "pass_needs_review"
	ClassificationFailed          Classification = "failed"
	ClassificationInconclusive    Classification = "inconclusive"
)

// HumanOutcome is the human judgment on the run's result. Receipts are
// written at finish time, so they normally start as "unreviewed"; later
// judgment arrives as an amendment.
type HumanOutcome string

const (
	HumanUnreviewed HumanOutcome = "unreviewed"
	HumanAccepted   HumanOutcome = "accepted"
	HumanRejected   HumanOutcome = "rejected"
	HumanReverted   HumanOutcome = "reverted"
)

// Repo pins the exact repository state the run started from and ended at.
type Repo struct {
	Identity        string `json:"identity"`          // canonical repo identity (module path or origin URL)
	BaseCommit      string `json:"base_commit"`       // commit the checkout was based on
	InitialTreeHash string `json:"initial_tree_hash"` // working-tree state hash before the run (captures dirty state)
	FinalTreeHash   string `json:"final_tree_hash"`   // working-tree state hash after the run
}

// Policy records the owner-policy evaluation for this run: what was allowed,
// how the restriction was enforced, and the verdict the core emitted.
type Policy struct {
	PolicyHash          string         `json:"policy_hash"`           // sha256 of the policy document applied
	Enforcement         Enforcement    `json:"enforcement"`           // how restrictions were enforced (fail-closed contract)
	EligibleCatalogHash string         `json:"eligible_catalog_hash"` // sha256 of the model catalog left after policy filtering
	Decision            PolicyDecision `json:"decision"`
}

// SelectionMode records how the attempt's model came to be chosen. An empty
// requested_model alone proves nothing; the mode makes the claim explicit.
type SelectionMode string

const (
	SelectionAuto           SelectionMode = "auto"            // the surface's router chose (e.g. Copilot Auto)
	SelectionExplicit       SelectionMode = "explicit"        // NeuroFS policy pinned a model
	SelectionSurfaceDefault SelectionMode = "surface_default" // the surface's configured default applied
	SelectionUnknown        SelectionMode = "unknown"
)

// ModelObservation is one sighting of a model identity during an attempt.
// A router may switch models mid-session, so resolved model is a collection
// of observations, never a scalar.
type ModelObservation struct {
	Model      string     `json:"model"`                 // model id as observed, verbatim
	Source     string     `json:"source"`                // where it was seen, e.g. "cli_stats_footer", "api_response"
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Attempt is one external launch (or escalation) within a run.
type Attempt struct {
	AttemptID         string             `json:"attempt_id"`
	Surface           string             `json:"surface"`        // e.g. "copilot_cli", "claude_cli", "api", "local"
	SelectionMode     SelectionMode      `json:"selection_mode"` // how the model was chosen; "explicit" requires requested_model
	RequestedModel    string             `json:"requested_model,omitempty"` // required iff selection_mode is "explicit"
	Argv              []string           `json:"argv"`                      // exact argument vector, never a shell string
	Exit              *int               `json:"exit,omitempty"`            // nil means the process never yielded an exit code
	DurationMS        int64              `json:"duration_ms"`
	ModelObservations []ModelObservation `json:"model_observations,omitempty"`
}

// ContextBundle pins the exact context NeuroFS produced for the run. The
// receipt always fixes bundle_path + bundle_hash; correlating by "newest
// bundle in the cache" is forbidden (it races).
type ContextBundle struct {
	BundlePath string     `json:"bundle_path"` // repo-relative path to the saved bundle
	BundleHash string     `json:"bundle_hash"`
	Tokens     *int       `json:"tokens,omitempty"` // nil iff provenance is "unknown" — absence never means zero
	Provenance Provenance `json:"provenance"`
}

// Usage is one metered figure. Quantity is a decimal string; when the value
// is unknown it stays empty with provenance "unknown" — never zero.
type Usage struct {
	Metric     string     `json:"metric"`             // e.g. "prompt_tokens", "premium_requests", "usd"
	Quantity   string     `json:"quantity,omitempty"` // decimal string; empty only when provenance is "unknown"
	Unit       string     `json:"unit"`               // e.g. "tokens", "requests", "usd"
	Provenance Provenance `json:"provenance"`
	Confidence Confidence `json:"confidence"`
}

// VerifierCommand freezes the verifier invocation as data: argument vector,
// working directory and timeout — never a shell string rediscovered later.
type VerifierCommand struct {
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	TimeoutMS int64    `json:"timeout_ms"`
}

// VerifierRun is one execution of the frozen verifier. TestsRun counts tests
// actually executed, excluding skipped ones.
type VerifierRun struct {
	Command      VerifierCommand `json:"command"`
	Exit         *int            `json:"exit,omitempty"` // nil means it never yielded an exit code
	TestsRun     int             `json:"tests_run"`
	TestsFailed  int             `json:"tests_failed"`
	TestsSkipped int             `json:"tests_skipped"`
	DurationMS   int64           `json:"duration_ms"`
	OutputSHA256 string          `json:"output_sha256,omitempty"`
}

// Integrity records whether the verifier surface (tests, fixtures, snapshots,
// runner, configuration) survived the run untouched. Hashes are computed over
// the real files on disk, not over indexed chunks: the index deliberately
// excludes some testdata, fixtures and formats that matter here.
type Integrity struct {
	VerifierSurfacePreSHA256  string   `json:"verifier_surface_pre_sha256"`
	VerifierSurfacePostSHA256 string   `json:"verifier_surface_post_sha256"`
	TouchedVerifierPaths      []string `json:"touched_verifier_paths,omitempty"`
}

// Verification carries the frozen task spec, the before/after verifier runs,
// the surface integrity check, and the resulting classification.
type Verification struct {
	TaskSpecHash   string         `json:"task_spec_hash"` // sha256 of the task specification
	Baseline       *VerifierRun   `json:"baseline,omitempty"`
	Final          *VerifierRun   `json:"final,omitempty"`
	Integrity      *Integrity     `json:"integrity,omitempty"`
	Classification Classification `json:"classification"`
}

// Artifact references a produced file by repo-relative path and content hash.
type Artifact struct {
	Kind   string `json:"kind"` // e.g. "diff", "agent_output", "verifier_log"
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Record is one line of the run-receipts ledger: either a run_receipt or a
// run_amendment, discriminated by RecordKind. Validation enforces per-kind
// required and forbidden fields.
//
// Field order is part of the content-hash contract (the hash covers the
// compact JSON encoding of this struct): do not reorder fields.
type Record struct {
	SchemaVersion int        `json:"schema_version"`
	RecordKind    RecordKind `json:"record_kind"`
	ReceiptID     string     `json:"receipt_id"`

	// run_receipt fields.
	TaskID       string          `json:"task_id,omitempty"` // groups comparable runs
	RunID        string          `json:"run_id,omitempty"`  // one full NeuroFS-controlled execution
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	Repo         *Repo           `json:"repo,omitempty"`
	Policy       *Policy         `json:"policy,omitempty"`
	Attempts     []Attempt       `json:"attempts,omitempty"`
	Context      []ContextBundle `json:"context,omitempty"`
	Usage        []Usage         `json:"usage,omitempty"` // also allowed on amendments: billing data arrives late
	Verification *Verification   `json:"verification,omitempty"`
	Artifacts    []Artifact      `json:"artifacts,omitempty"`
	HumanOutcome HumanOutcome    `json:"human_outcome,omitempty"`

	// run_amendment fields.
	CorrectsReceiptID string         `json:"corrects_receipt_id,omitempty"`
	CreatedAt         *time.Time     `json:"created_at,omitempty"`
	Classification    Classification `json:"classification,omitempty"` // amendment-level reclassification
	Note              string         `json:"note,omitempty"`

	// ContentSHA256 makes edits to the JSONL file detectable: it is the
	// sha256 of this record's compact JSON encoding with this field empty.
	ContentSHA256 string `json:"content_sha256"`
}
