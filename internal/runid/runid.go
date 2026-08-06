// Package runid carries the run identity that correlates every artifact of
// one NeuroFS-controlled run: context bundles, ledger entries, usage,
// grounding, audit and the final RunReceipt.
//
// Three rules shape this package:
//
//   - The identity is read once and propagated immutably. It travels down
//     through a context.Context and outward to child processes through an
//     explicit cmd.Env slice. os.Setenv is never called: mutating the
//     process environment would make the identity ambient, racy and shared
//     with unrelated code.
//   - An explicit identity and an ambient one that disagree is an error, not
//     a precedence puzzle. Silently preferring one would silently mislabel
//     every artifact of the run.
//   - Correlation coverage is declared, never assumed. Environment
//     propagation only identifies a run while the adapter owns the process
//     tree: adapter → agent CLI → stdio MCP server, all launched for this
//     one run. A long-lived or shared server cannot identify the current
//     request's run from the environment it was launched with, and must
//     report CorrelationUnavailable rather than attach a stale id. See
//     Availability.
//
// run_id does not replace session_id: a session groups a human's working
// context over time, a run is one controlled execution. Both are recorded.
package runid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Gere2/neurofs/internal/receipt"
)

// EnvVar is the environment variable that carries the run identity to child
// processes. It is only ever written into an explicit cmd.Env slice.
const EnvVar = "NEUROFS_RUN_ID"

// idPrefix makes a run id recognizable in logs and ledgers at a glance.
const idPrefix = "run-"

// randomBytes is the entropy per generated id (128 bits).
const randomBytes = 16

// RunID is a validated run identity. The zero value means "no run identity";
// every non-zero RunID has passed Parse and is writable into a RunReceipt.
type RunID string

// New generates a fresh run identity.
func New() (RunID, error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("runid: generate: %w", err)
	}
	return Parse(idPrefix + hex.EncodeToString(buf))
}

// Parse validates s as a run identity. The rule is the receipt identifier
// rule, single-sourced from the ledger schema: an id that cannot be written
// into a receipt is not a usable correlation key.
func Parse(s string) (RunID, error) {
	if s == "" {
		return "", fmt.Errorf("runid: empty run id")
	}
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("runid: run id must be one line")
	}
	if s != strings.TrimSpace(s) {
		return "", fmt.Errorf("runid: run id %q has surrounding whitespace", s)
	}
	if !receipt.ValidIdentifier(s) {
		return "", fmt.Errorf("runid: run id %q must match %s", s, receipt.IdentifierPattern())
	}
	if !strings.HasPrefix(s, idPrefix) {
		return "", fmt.Errorf("runid: run id %q must start with %q", s, idPrefix)
	}
	return RunID(s), nil
}

// String returns the identity as it is written to receipts and environments.
func (id RunID) String() string { return string(id) }

// IsZero reports whether there is no run identity.
func (id RunID) IsZero() bool { return id == "" }
