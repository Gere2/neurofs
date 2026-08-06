package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ComputeContentSHA256 returns the record hash: sha256 over the compact JSON
// encoding of the record with ContentSHA256 set to "". The field itself has
// no omitempty, so the preimage literally contains `"content_sha256":""`.
//
// The hash input is the v1 struct's encoding: field order and the set of
// known fields are part of the contract. Records are decoded strictly
// (DecodeRecord rejects unknown fields), so a record can never carry data
// the hash does not cover; future schemas are gated by schema_version, not
// by silently tolerated extra fields.
func ComputeContentSHA256(r Record) (string, error) {
	r.ContentSHA256 = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("receipt: encode for hashing: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Seal computes and stores the record's ContentSHA256. Writers call it once,
// immediately before appending the serialized line to the ledger.
func (r *Record) Seal() error {
	h, err := ComputeContentSHA256(*r)
	if err != nil {
		return err
	}
	r.ContentSHA256 = h
	return nil
}

// VerifyContentSHA256 recomputes the record hash and compares it with the
// stored one, making in-place edits to the JSONL ledger detectable.
func (r *Record) VerifyContentSHA256() error {
	if r.ContentSHA256 == "" {
		return fmt.Errorf("receipt %s: content_sha256 missing", r.ReceiptID)
	}
	want, err := ComputeContentSHA256(*r)
	if err != nil {
		return err
	}
	if want != r.ContentSHA256 {
		return fmt.Errorf("receipt %s: content_sha256 mismatch: record does not match its hash", r.ReceiptID)
	}
	return nil
}
