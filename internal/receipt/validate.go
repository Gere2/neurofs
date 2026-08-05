package receipt

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var (
	// idRe keeps identifiers single-line, printable and join-safe.
	idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	// sha256Re matches lowercase hex sha256 digests (hashes NeuroFS computes).
	sha256Re = regexp.MustCompile(`^[a-f0-9]{64}$`)
	// decimalRe matches non-negative decimal strings. No floats, no exponents.
	decimalRe = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
)

// Validate checks a single record against the v1 schema, including the
// kind-dependent required/forbidden fields and the clean_pass_at_1
// cross-rules. It does not verify ContentSHA256 against the record content;
// use VerifyContentSHA256 for tamper detection.
func (r *Record) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version: got %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if err := validID("receipt_id", r.ReceiptID); err != nil {
		return err
	}
	if !sha256Re.MatchString(r.ContentSHA256) {
		return fmt.Errorf("content_sha256: must be 64 lowercase hex chars")
	}
	switch r.RecordKind {
	case KindRunReceipt:
		return r.validateReceipt()
	case KindRunAmendment:
		return r.validateAmendment()
	default:
		return fmt.Errorf("record_kind: unknown %q", r.RecordKind)
	}
}

func (r *Record) validateReceipt() error {
	// Amendment-only fields are forbidden on receipts.
	if r.CorrectsReceiptID != "" {
		return fmt.Errorf("run_receipt: corrects_receipt_id is amendment-only")
	}
	if r.CreatedAt != nil {
		return fmt.Errorf("run_receipt: created_at is amendment-only")
	}
	if r.Classification != "" {
		return fmt.Errorf("run_receipt: top-level classification is amendment-only (use verification.classification)")
	}
	if r.Note != "" {
		return fmt.Errorf("run_receipt: note is amendment-only")
	}

	if err := validID("task_id", r.TaskID); err != nil {
		return err
	}
	if err := validID("run_id", r.RunID); err != nil {
		return err
	}
	if r.StartedAt == nil || r.FinishedAt == nil {
		return fmt.Errorf("run_receipt: started_at and finished_at are required")
	}
	if r.FinishedAt.Before(*r.StartedAt) {
		return fmt.Errorf("run_receipt: finished_at precedes started_at")
	}

	if r.Repo == nil {
		return fmt.Errorf("run_receipt: repo is required")
	}
	for field, v := range map[string]string{
		"repo.identity":          r.Repo.Identity,
		"repo.base_commit":       r.Repo.BaseCommit,
		"repo.initial_tree_hash": r.Repo.InitialTreeHash,
		"repo.final_tree_hash":   r.Repo.FinalTreeHash,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s: required", field)
		}
	}

	if r.Policy == nil {
		return fmt.Errorf("run_receipt: policy is required")
	}
	if !sha256Re.MatchString(r.Policy.PolicyHash) {
		return fmt.Errorf("policy.policy_hash: must be sha256 hex")
	}
	if !validEnforcement(r.Policy.Enforcement) {
		return fmt.Errorf("policy.enforcement: unknown %q", r.Policy.Enforcement)
	}
	if !sha256Re.MatchString(r.Policy.EligibleCatalogHash) {
		return fmt.Errorf("policy.eligible_catalog_hash: must be sha256 hex")
	}
	if !validDecision(r.Policy.Decision) {
		return fmt.Errorf("policy.decision: unknown %q", r.Policy.Decision)
	}
	if r.Policy.Decision == DecisionDeny && len(r.Attempts) > 0 {
		return fmt.Errorf("policy.decision: deny forbids attempts")
	}

	seenAttempts := make(map[string]bool, len(r.Attempts))
	for i, a := range r.Attempts {
		if err := validAttempt(i, a); err != nil {
			return err
		}
		if seenAttempts[a.AttemptID] {
			return fmt.Errorf("attempts[%d].attempt_id: duplicate %q", i, a.AttemptID)
		}
		seenAttempts[a.AttemptID] = true
	}

	for i, c := range r.Context {
		if err := validRelPath(fmt.Sprintf("context[%d].bundle_path", i), c.BundlePath); err != nil {
			return err
		}
		if strings.TrimSpace(c.BundleHash) == "" {
			return fmt.Errorf("context[%d].bundle_hash: required", i)
		}
		if !validProvenance(c.Provenance) {
			return fmt.Errorf("context[%d].provenance: unknown %q", i, c.Provenance)
		}
		if c.Provenance == ProvenanceUnknown {
			if c.Tokens != nil {
				return fmt.Errorf("context[%d].tokens: must be absent when provenance is unknown (absence never means zero)", i)
			}
		} else {
			if c.Tokens == nil {
				return fmt.Errorf("context[%d].tokens: required when provenance is %q", i, c.Provenance)
			}
			if *c.Tokens < 0 {
				return fmt.Errorf("context[%d].tokens: negative", i)
			}
		}
	}

	for i, u := range r.Usage {
		if err := validUsage(i, u); err != nil {
			return err
		}
	}

	if r.Verification == nil {
		return fmt.Errorf("run_receipt: verification is required")
	}
	if err := r.validateVerification(); err != nil {
		return err
	}

	for i, a := range r.Artifacts {
		if strings.TrimSpace(a.Kind) == "" {
			return fmt.Errorf("artifacts[%d].kind: required", i)
		}
		if err := validRelPath(fmt.Sprintf("artifacts[%d].path", i), a.Path); err != nil {
			return err
		}
		if !sha256Re.MatchString(a.SHA256) {
			return fmt.Errorf("artifacts[%d].sha256: must be sha256 hex", i)
		}
	}

	if !validHumanOutcome(r.HumanOutcome) {
		return fmt.Errorf("human_outcome: unknown %q", r.HumanOutcome)
	}
	return nil
}

func (r *Record) validateVerification() error {
	v := r.Verification
	if !sha256Re.MatchString(v.TaskSpecHash) {
		return fmt.Errorf("verification.task_spec_hash: must be sha256 hex")
	}
	if !validClassification(v.Classification) {
		return fmt.Errorf("verification.classification: unknown %q", v.Classification)
	}
	if v.Baseline != nil {
		if err := validVerifierRun("verification.baseline", v.Baseline); err != nil {
			return err
		}
	}
	if v.Final != nil {
		if err := validVerifierRun("verification.final", v.Final); err != nil {
			return err
		}
	}
	if v.Integrity != nil {
		if !sha256Re.MatchString(v.Integrity.VerifierSurfacePreSHA256) {
			return fmt.Errorf("verification.integrity.verifier_surface_pre_sha256: must be sha256 hex")
		}
		if !sha256Re.MatchString(v.Integrity.VerifierSurfacePostSHA256) {
			return fmt.Errorf("verification.integrity.verifier_surface_post_sha256: must be sha256 hex")
		}
		for i, p := range v.Integrity.TouchedVerifierPaths {
			if err := validRelPath(fmt.Sprintf("verification.integrity.touched_verifier_paths[%d]", i), p); err != nil {
				return err
			}
		}
	}
	if v.Classification == ClassificationCleanPass {
		return r.validateCleanPass()
	}
	return nil
}

// validateCleanPass enforces the clean_pass_at_1 contract: reproducible
// failing baseline, exactly one external attempt, the verifier really
// executed and passed, and an identical frozen verifier surface.
func (r *Record) validateCleanPass() error {
	v := r.Verification
	if len(r.Attempts) != 1 {
		return fmt.Errorf("clean_pass_at_1: requires exactly one attempt, got %d", len(r.Attempts))
	}
	if v.Baseline == nil || v.Final == nil || v.Integrity == nil {
		return fmt.Errorf("clean_pass_at_1: baseline, final and integrity are required")
	}
	if v.Baseline.Exit == nil || *v.Baseline.Exit == 0 {
		return fmt.Errorf("clean_pass_at_1: baseline must exit non-zero — a passing or unfinished baseline reproduces nothing")
	}
	if v.Baseline.TestsFailed < 1 {
		return fmt.Errorf("clean_pass_at_1: baseline must reproduce the failure (tests_failed >= 1)")
	}
	if v.Final.Exit == nil || *v.Final.Exit != 0 {
		return fmt.Errorf("clean_pass_at_1: final verifier must exit 0")
	}
	if v.Final.TestsRun < 1 {
		return fmt.Errorf("clean_pass_at_1: exit 0 with zero executed tests does not count")
	}
	if v.Final.TestsFailed != 0 {
		return fmt.Errorf("clean_pass_at_1: final verifier reports failures")
	}
	if !verifierCommandsEqual(v.Baseline.Command, v.Final.Command) {
		return fmt.Errorf("clean_pass_at_1: baseline and final verifier commands differ")
	}
	if v.Integrity.VerifierSurfacePreSHA256 != v.Integrity.VerifierSurfacePostSHA256 ||
		len(v.Integrity.TouchedVerifierPaths) != 0 {
		return fmt.Errorf("clean_pass_at_1: verifier surface was modified; classify as pass_needs_review")
	}
	return nil
}

func (r *Record) validateAmendment() error {
	// Receipt-only fields are forbidden on amendments.
	forbidden := []struct {
		name string
		set  bool
	}{
		{"task_id", r.TaskID != ""},
		{"run_id", r.RunID != ""},
		{"started_at", r.StartedAt != nil},
		{"finished_at", r.FinishedAt != nil},
		{"repo", r.Repo != nil},
		{"policy", r.Policy != nil},
		{"attempts", len(r.Attempts) > 0},
		{"context", len(r.Context) > 0},
		{"verification", r.Verification != nil},
		{"artifacts", len(r.Artifacts) > 0},
	}
	for _, f := range forbidden {
		if f.set {
			return fmt.Errorf("run_amendment: %s is receipt-only", f.name)
		}
	}

	if err := validID("corrects_receipt_id", r.CorrectsReceiptID); err != nil {
		return err
	}
	if r.CorrectsReceiptID == r.ReceiptID {
		return fmt.Errorf("corrects_receipt_id: cannot correct itself")
	}
	if r.CreatedAt == nil {
		return fmt.Errorf("run_amendment: created_at is required")
	}

	hasCorrection := len(r.Usage) > 0 || r.HumanOutcome != "" || r.Classification != "" || r.Note != ""
	if !hasCorrection {
		return fmt.Errorf("run_amendment: must carry at least one correction (usage, human_outcome, classification or note)")
	}
	for i, u := range r.Usage {
		if err := validUsage(i, u); err != nil {
			return err
		}
	}
	if r.HumanOutcome != "" && !validHumanOutcome(r.HumanOutcome) {
		return fmt.Errorf("human_outcome: unknown %q", r.HumanOutcome)
	}
	if r.Classification != "" && !validClassification(r.Classification) {
		return fmt.Errorf("classification: unknown %q", r.Classification)
	}
	if r.Classification == ClassificationCleanPass {
		return fmt.Errorf("classification: an amendment cannot reclassify to clean_pass_at_1 — that verdict is earned mechanically by the receipt's verification; express human review as human_outcome accepted")
	}
	return nil
}

// ValidateSet validates each record and the cross-record invariants of a
// ledger: unique receipt_id, at most one run_receipt per run_id, and every
// amendment pointing at a run_receipt present in the set. It does not verify
// content hashes; call VerifyContentSHA256 per record for that.
func ValidateSet(records []Record) error {
	byReceiptID := make(map[string]RecordKind, len(records))
	receiptByRunID := make(map[string]string, len(records))

	for i := range records {
		r := &records[i]
		if err := r.Validate(); err != nil {
			return fmt.Errorf("records[%d]: %w", i, err)
		}
		if _, dup := byReceiptID[r.ReceiptID]; dup {
			return fmt.Errorf("records[%d]: duplicate receipt_id %q", i, r.ReceiptID)
		}
		byReceiptID[r.ReceiptID] = r.RecordKind
		if r.RecordKind == KindRunReceipt {
			if prev, dup := receiptByRunID[r.RunID]; dup {
				return fmt.Errorf("records[%d]: run_id %q already has receipt %q", i, r.RunID, prev)
			}
			receiptByRunID[r.RunID] = r.ReceiptID
		}
	}

	for i := range records {
		r := &records[i]
		if r.RecordKind != KindRunAmendment {
			continue
		}
		kind, ok := byReceiptID[r.CorrectsReceiptID]
		if !ok {
			return fmt.Errorf("records[%d]: corrects_receipt_id %q not found in set", i, r.CorrectsReceiptID)
		}
		if kind != KindRunReceipt {
			return fmt.Errorf("records[%d]: corrects_receipt_id %q is not a run_receipt", i, r.CorrectsReceiptID)
		}
	}
	return nil
}

func validAttempt(i int, a Attempt) error {
	if err := validID(fmt.Sprintf("attempts[%d].attempt_id", i), a.AttemptID); err != nil {
		return err
	}
	if strings.TrimSpace(a.Surface) == "" {
		return fmt.Errorf("attempts[%d].surface: required", i)
	}
	if !validSelectionMode(a.SelectionMode) {
		return fmt.Errorf("attempts[%d].selection_mode: unknown %q", i, a.SelectionMode)
	}
	switch a.SelectionMode {
	case SelectionExplicit:
		if a.RequestedModel == "" {
			return fmt.Errorf("attempts[%d].requested_model: required when selection_mode is explicit", i)
		}
	case SelectionAuto, SelectionSurfaceDefault:
		if a.RequestedModel != "" {
			return fmt.Errorf("attempts[%d].requested_model: must be empty when selection_mode is %q", i, a.SelectionMode)
		}
	}
	if len(a.Argv) == 0 || strings.TrimSpace(a.Argv[0]) == "" {
		return fmt.Errorf("attempts[%d].argv: required, argv[0] must be the executable", i)
	}
	if a.DurationMS < 0 {
		return fmt.Errorf("attempts[%d].duration_ms: negative", i)
	}
	for j, o := range a.ModelObservations {
		if strings.TrimSpace(o.Model) == "" {
			return fmt.Errorf("attempts[%d].model_observations[%d].model: required", i, j)
		}
		if strings.TrimSpace(o.Source) == "" {
			return fmt.Errorf("attempts[%d].model_observations[%d].source: required", i, j)
		}
	}
	return nil
}

func validUsage(i int, u Usage) error {
	if strings.TrimSpace(u.Metric) == "" {
		return fmt.Errorf("usage[%d].metric: required", i)
	}
	if strings.TrimSpace(u.Unit) == "" {
		return fmt.Errorf("usage[%d].unit: required", i)
	}
	if !validProvenance(u.Provenance) {
		return fmt.Errorf("usage[%d].provenance: unknown %q", i, u.Provenance)
	}
	if !validConfidence(u.Confidence) {
		return fmt.Errorf("usage[%d].confidence: unknown %q", i, u.Confidence)
	}
	if u.Provenance == ProvenanceUnknown {
		if u.Quantity != "" {
			return fmt.Errorf("usage[%d].quantity: must be empty when provenance is unknown (absence never means zero)", i)
		}
		return nil
	}
	if !decimalRe.MatchString(u.Quantity) {
		return fmt.Errorf("usage[%d].quantity: %q is not a non-negative decimal string", i, u.Quantity)
	}
	return nil
}

func validVerifierRun(field string, vr *VerifierRun) error {
	if len(vr.Command.Argv) == 0 || strings.TrimSpace(vr.Command.Argv[0]) == "" {
		return fmt.Errorf("%s.command.argv: required, argv[0] must be the executable", field)
	}
	if strings.TrimSpace(vr.Command.Cwd) == "" {
		return fmt.Errorf("%s.command.cwd: required", field)
	}
	if vr.Command.TimeoutMS <= 0 {
		return fmt.Errorf("%s.command.timeout_ms: must be positive (the timeout is part of the frozen verifier)", field)
	}
	if vr.TestsRun < 0 || vr.TestsFailed < 0 || vr.TestsSkipped < 0 {
		return fmt.Errorf("%s: negative test counts", field)
	}
	if vr.TestsFailed > vr.TestsRun {
		return fmt.Errorf("%s: tests_failed exceeds tests_run", field)
	}
	if vr.DurationMS < 0 {
		return fmt.Errorf("%s.duration_ms: negative", field)
	}
	if vr.OutputSHA256 != "" && !sha256Re.MatchString(vr.OutputSHA256) {
		return fmt.Errorf("%s.output_sha256: must be sha256 hex", field)
	}
	return nil
}

func verifierCommandsEqual(a, b VerifierCommand) bool {
	if a.Cwd != b.Cwd || a.TimeoutMS != b.TimeoutMS || len(a.Argv) != len(b.Argv) {
		return false
	}
	for i := range a.Argv {
		if a.Argv[i] != b.Argv[i] {
			return false
		}
	}
	return true
}

// ValidIdentifier reports whether s is a well-formed receipt identifier
// (receipt_id, task_id, run_id, attempt_id). Exported so producers of those
// identifiers validate against the same rule the ledger enforces instead of
// duplicating the pattern: an id that cannot be written into a receipt is
// useless as a correlation key.
func ValidIdentifier(s string) bool {
	return idRe.MatchString(s)
}

// IdentifierPattern is the identifier rule, for error messages.
func IdentifierPattern() string {
	return idRe.String()
}

func validID(field, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("%s: %q must match %s", field, id, idRe.String())
	}
	return nil
}

// validRelPath enforces repo confinement: relative, slash-separated, no
// escapes. Receipts reference artifacts inside the repository only.
func validRelPath(field, p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%s: required", field)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("%s: %q must be a relative slash-separated path", field, p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s: %q escapes the repository", field, p)
	}
	return nil
}

func validEnforcement(e Enforcement) bool {
	switch e {
	case EnforcementLocal, EnforcementProvider, EnforcementObserved, EnforcementUnknown:
		return true
	}
	return false
}

func validDecision(d PolicyDecision) bool {
	switch d {
	case DecisionAllow, DecisionDeny, DecisionRecommend, DecisionEscalate:
		return true
	}
	return false
}

func validProvenance(p Provenance) bool {
	switch p {
	case ProvenanceProviderReported, ProvenanceObserved, ProvenanceEstimated, ProvenanceUnknown:
		return true
	}
	return false
}

func validConfidence(c Confidence) bool {
	switch c {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow, ConfidenceUnknown:
		return true
	}
	return false
}

func validSelectionMode(m SelectionMode) bool {
	switch m {
	case SelectionAuto, SelectionExplicit, SelectionSurfaceDefault, SelectionUnknown:
		return true
	}
	return false
}

func validClassification(c Classification) bool {
	switch c {
	case ClassificationCleanPass, ClassificationPassNeedsReview, ClassificationFailed, ClassificationInconclusive:
		return true
	}
	return false
}

func validHumanOutcome(h HumanOutcome) bool {
	switch h {
	case HumanUnreviewed, HumanAccepted, HumanRejected, HumanReverted:
		return true
	}
	return false
}
