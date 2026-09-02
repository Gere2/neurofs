package kernel

import (
	"fmt"
	"strings"
)

// EvidenceRef points at bytes NeuroFS does not own. The system of record is
// always outside this process (git+working tree, a future business SoR, a
// saved bundle on disk). Locator is opaque to the plane: adapters interpret it.
type EvidenceRef struct {
	// System names the SoR or artefact store, e.g. "git+fs", "audit_bundle",
	// "run_receipt". It is not a NeuroFS table name.
	System string `json:"system"`
	// Locator is how to find the evidence in that system (path#line, document
	// id, receipt_id). It is a pointer, not a copy of the record.
	Locator string `json:"locator"`
	// ContentSHA256 is the digest of the observed bytes when known. Empty
	// means unknown — never treat empty as "matches anything".
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

// Validate requires a system and locator. A hash, when present, must be
// lowercase hex sha256.
func (e EvidenceRef) Validate() error {
	if strings.TrimSpace(e.System) == "" {
		return fmt.Errorf("kernel: evidence system is required")
	}
	if strings.TrimSpace(e.Locator) == "" {
		return fmt.Errorf("kernel: evidence locator is required")
	}
	if e.ContentSHA256 != "" && !sha256Hex.MatchString(e.ContentSHA256) {
		return fmt.Errorf("kernel: evidence content_sha256 must be 64 lowercase hex chars")
	}
	return nil
}
