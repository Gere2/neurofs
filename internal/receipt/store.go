package receipt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LedgerRelPath is the canonical run-receipts ledger, relative to the repo
// root. The JSONL file is the source of truth; any index over it is derived.
const LedgerRelPath = "audit/run_receipts.jsonl"

// LedgerPath returns the absolute path of the ledger for a repo root.
func LedgerPath(repoRoot string) string {
	return filepath.Join(repoRoot, "audit", "run_receipts.jsonl")
}

// AppendRecord validates, seals and appends r to the repo's ledger as one
// operation: snapshot → seal → validate → preflight the existing ledger →
// single append write → sync. On success r.ContentSHA256 is set to the
// sealed hash.
//
// Confinement and the write path share one descriptor: the file is opened
// through os.Root (no path component may escape the repository, including a
// symlinked audit/ directory), and that same O_RDWR|O_APPEND descriptor is
// locked, read, validated and written — there is no window to validate one
// inode and write another.
//
// The preflight is complete: decode, per-line hash verification and set
// validation all run under the exclusive lock, so an append can never extend
// a corrupted or inconsistent ledger — a tampered line, a duplicate run_id or
// a dangling amendment refuses the write before any byte lands. O(n) per
// append is accepted for v1; scale belongs to the derived index, not to the
// source of truth.
//
// The adapter is the single canonical writer of final receipts; the lock
// defends against accidental concurrent appends, not against a hostile
// writer (see the package note on integrity scope).
func AppendRecord(repoRoot string, r *Record) error {
	if strings.TrimSpace(repoRoot) == "" {
		return fmt.Errorf("receipt: append: repo root required")
	}

	snapshot := *r
	snapshot.ContentSHA256 = ""
	if err := snapshot.Seal(); err != nil {
		return err
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("receipt: append: %w", err)
	}
	line, err := marshalLine(snapshot)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return fmt.Errorf("receipt: append: %w", err)
	}
	defer root.Close()
	if err := root.Mkdir("audit", 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("receipt: append: %w", err)
	}
	f, err := openLedgerForAppend(root)
	if err != nil {
		return fmt.Errorf("receipt: append: %w", err)
	}
	defer f.Close()
	if err := ensureRegular(f); err != nil {
		return fmt.Errorf("receipt: append: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("receipt: append: %w", err)
	}
	if err := lockFile(f, true); err != nil {
		return fmt.Errorf("receipt: append: lock ledger: %w", err)
	}
	defer func() { _ = unlockFile(f) }()

	existing, err := decodeLedger(f)
	if err != nil {
		return fmt.Errorf("receipt: append: existing ledger is not readable, refusing to extend it: %w", err)
	}
	for i := range existing {
		if err := existing[i].VerifyContentSHA256(); err != nil {
			return fmt.Errorf("receipt: append: line %d: %w — refusing to extend a corrupted ledger", i+1, err)
		}
	}
	if err := ValidateSet(append(existing, snapshot)); err != nil {
		return fmt.Errorf("receipt: append: %w", err)
	}

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("receipt: append: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("receipt: append: sync: %w", err)
	}
	r.ContentSHA256 = snapshot.ContentSHA256
	return nil
}

// LoadLedger reads the repo's ledger under a shared lock, decoding strictly
// and canonically, verifying every record's content hash and the
// cross-record invariants. A missing ledger is an empty ledger. A torn or
// edited line is surfaced as an error with its 1-based line number —
// recovery is a human decision; the ledger is append-only history and is
// never auto-pruned or rewritten.
func LoadLedger(repoRoot string) ([]Record, error) {
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("receipt: load: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(LedgerRelPath, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("receipt: load: %w", err)
	}
	defer f.Close()
	if err := ensureRegular(f); err != nil {
		return nil, fmt.Errorf("receipt: load: %w", err)
	}
	if err := lockFile(f, false); err != nil {
		return nil, fmt.Errorf("receipt: load: lock ledger: %w", err)
	}
	defer func() { _ = unlockFile(f) }()

	records, err := decodeLedger(f)
	if err != nil {
		return nil, fmt.Errorf("receipt: load: %w", err)
	}
	for i := range records {
		if err := records[i].VerifyContentSHA256(); err != nil {
			return nil, fmt.Errorf("receipt: load: line %d: %w", i+1, err)
		}
	}
	if err := ValidateSet(records); err != nil {
		return nil, fmt.Errorf("receipt: load: %w", err)
	}
	return records, nil
}

// decodeLedger applies strict JSONL framing: the ledger is a sequence of
// canonical records each terminated by exactly one '\n'. Only a zero-byte
// file is an empty ledger — a whitespace-only file is malformed, not empty.
// A missing final newline means the tail was truncated (the writer always
// emits it), and blank lines are rejected rather than skipped.
func decodeLedger(rd io.Reader) ([]Record, error) {
	raw, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != '\n' {
		return nil, fmt.Errorf("ledger does not end with a newline: the final record is truncated")
	}
	lines := bytes.Split(raw[:len(raw)-1], []byte("\n"))
	records := make([]Record, 0, len(lines))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("line %d: blank line", i+1)
		}
		r, err := DecodeRecord(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		records = append(records, r)
	}
	return records, nil
}

// openLedgerForAppend opens the ledger through the root, retrying spurious
// ENOENT: os.Root's emulated multi-component resolution can fail transiently
// when several processes create the same path concurrently (reproduced on
// darwin/go1.26: ENOENT returned with the directory demonstrably present —
// the internal create-vs-open loop gives up under contention). The bounded
// backoff converts that transient failure into the open that was asked for;
// any other error returns immediately.
func openLedgerForAppend(root *os.Root) (*os.File, error) {
	var lastErr error
	for i := 0; i < 8; i++ {
		f, err := root.OpenFile(LedgerRelPath, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		lastErr = err
		time.Sleep(time.Millisecond << uint(i))
	}
	return nil, lastErr
}

func ensureRegular(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", f.Name())
	}
	return nil
}

func marshalLine(r Record) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("receipt: encode: %w", err)
	}
	return append(b, '\n'), nil
}
