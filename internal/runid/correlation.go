package runid

import (
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// Correlation declares whether run correlation can be trusted in this process.
// It is recorded, not guessed: a consumer reading an artifact must be able to
// tell a real correlation from a missing one.
type Correlation string

const (
	// CorrelationOwnedProcessTree means the identity arrived through an
	// environment the adapter set for this run, down a process tree it owns:
	// adapter → agent CLI → stdio MCP server, all launched for this one run.
	CorrelationOwnedProcessTree Correlation = "owned_process_tree"
	// CorrelationUnavailable means this process cannot attribute its work to
	// a run. Artifacts are still produced; they are simply not correlated.
	CorrelationUnavailable Correlation = "unavailable"
)

// Availability is the declared correlation coverage of the current process.
type Availability struct {
	RunID       RunID       `json:"run_id,omitempty"`
	Correlation Correlation `json:"run_correlation,omitempty"`
	// Reason explains an unavailable correlation in one line, so the gap is
	// diagnosable from the artifact rather than from tribal knowledge.
	Reason string `json:"run_correlation_reason,omitempty"`
}

// Available reports whether artifacts produced here can carry a run identity.
func (a Availability) Available() bool {
	return a.Correlation == CorrelationOwnedProcessTree && a.Validate() == nil
}

// Validate rejects internally contradictory availability declarations. RunID
// is a string-backed type and callers can construct it directly, so every
// trust boundary validates instead of assuming the type alone is proof.
func (a Availability) Validate() error {
	switch a.Correlation {
	case CorrelationOwnedProcessTree:
		if a.RunID.IsZero() {
			return fmt.Errorf("runid: owned process-tree correlation requires run_id")
		}
		if _, err := Parse(a.RunID.String()); err != nil {
			return fmt.Errorf("runid: availability: %w", err)
		}
	case CorrelationUnavailable:
		if !a.RunID.IsZero() {
			return fmt.Errorf("runid: unavailable correlation must not carry run_id %q", a.RunID)
		}
		if strings.TrimSpace(a.Reason) == "" {
			return fmt.Errorf("runid: unavailable correlation requires a reason")
		}
	default:
		return fmt.Errorf("runid: unsupported correlation %q", a.Correlation)
	}
	return nil
}

// ForOwnedProcessTree declares the supported topology: a one-shot process
// tree the adapter launched and owns for a single run. The ambient identity is
// trustworthy here precisely because the environment was set for this run and
// the process does not outlive it.
func ForOwnedProcessTree() Availability {
	id, set, err := FromEnv()
	switch {
	case err != nil:
		return Availability{Correlation: CorrelationUnavailable, Reason: err.Error()}
	case !set:
		return Availability{
			Correlation: CorrelationUnavailable,
			Reason:      fmt.Sprintf("%s is not set: this process was not launched as part of a NeuroFS run", EnvVar),
		}
	default:
		return Availability{RunID: id, Correlation: CorrelationOwnedProcessTree}
	}
}

// ForPersistentServer declares the unsupported topology. A long-lived or
// shared server — a persistent MCP server, a remote one, one registered once
// and reused across runs — cannot identify the current request's run from the
// environment it was launched with: that environment is either absent or
// stale, and attaching a stale id is worse than attaching none. Correlation
// stays unavailable until the identity travels request-scoped, per call.
//
// The ambient environment is deliberately not consulted.
func ForPersistentServer() Availability {
	return Availability{
		Correlation: CorrelationUnavailable,
		Reason: "persistent or shared server: launch environment cannot identify the current request's run; " +
			"needs request-scoped correlation",
	}
}

// JoinKey is the only legitimate way to correlate an artifact with the context
// that produced it: the run identity plus the exact bundle, pinned by path and
// content hash.
//
// Joining by "the newest bundle in the cache" or by timestamp proximity is
// forbidden — both race under concurrent runs and silently attribute one run's
// evidence to another. A partial key is refused rather than completed by
// guesswork.
type JoinKey struct {
	RunID      RunID  `json:"run_id"`
	BundlePath string `json:"bundle_path"`
	BundleHash string `json:"bundle_hash"`
}

// Validate reports whether the key identifies exactly one run and bundle.
func (k JoinKey) Validate() error {
	if k.RunID.IsZero() {
		return fmt.Errorf("runid: join key: run_id required")
	}
	if _, err := Parse(k.RunID.String()); err != nil {
		return fmt.Errorf("runid: join key: %w", err)
	}
	if strings.TrimSpace(k.BundlePath) == "" {
		return fmt.Errorf("runid: join key: bundle_path required (never join by newest bundle)")
	}
	if k.BundlePath != strings.TrimSpace(k.BundlePath) ||
		strings.Contains(k.BundlePath, "\\") ||
		path.IsAbs(k.BundlePath) ||
		path.Clean(k.BundlePath) != k.BundlePath ||
		k.BundlePath == "." || k.BundlePath == ".." ||
		strings.HasPrefix(k.BundlePath, "../") {
		return fmt.Errorf("runid: join key: bundle_path must be a clean, repo-relative slash-separated path")
	}
	if k.BundleHash == "" {
		return fmt.Errorf("runid: join key: bundle_hash required (path alone does not pin content)")
	}
	decoded, err := hex.DecodeString(k.BundleHash)
	if err != nil || len(decoded) != 32 || strings.ToLower(k.BundleHash) != k.BundleHash {
		return fmt.Errorf("runid: join key: bundle_hash must be lowercase sha256 hex")
	}
	return nil
}
