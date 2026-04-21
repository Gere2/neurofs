package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultRecordsDir is the location where audit records live inside a repo.
// It is relative to the repo root, not NeuroFS's own config dir, so the
// records stay in version control if the user wants — they're meant to be
// shared artefacts, like the benchmark file.
const DefaultRecordsDir = "audit/records"

// SaveRecord writes rec to dir as a JSON file and returns the resulting
// path. Filename is `<unix-sec>-<shorthash>-<rand6>.json` so records
// sort chronologically AND never collide — the random suffix defends
// against two runs of the same bundle within the same wall-clock
// second, a case the old `<unix-sec>-<shorthash>.json` scheme silently
// overwrote.
//
// The caller owns dir — SaveRecord creates it (MkdirAll) if missing. That
// means first-time use works without the user doing anything. Legacy
// records written by earlier versions stay on disk untouched; LoadRecord
// still reads them, and ListRecords still finds them — the filename
// change only affects new writes.
func SaveRecord(dir string, rec AuditRecord) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("audit: SaveRecord: dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	ts := rec.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	short := rec.BundleHash
	if len(short) > 8 {
		short = short[:8]
	}
	name := fmt.Sprintf("%d-%s-%s.json", ts.Unix(), short, randSuffix(3))
	path := filepath.Join(dir, name)

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("audit: marshal record: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("audit: write record: %w", err)
	}
	return path, nil
}

// randSuffix returns 2n hex characters of cryptographic entropy. We use
// crypto/rand rather than math/rand so two concurrent SaveRecord calls
// never collide — and so we don't need to seed anything. The fallback
// path covers the extremely rare case where /dev/urandom is unavailable;
// ts.UnixNano is still monotonically advancing in practice so it gives
// per-call uniqueness even when crypto/rand is denied.
func randSuffix(n int) string {
	if n <= 0 {
		n = 3
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	// Fallback: last 2n hex digits of the current wall-clock in ns.
	ns := fmt.Sprintf("%x", time.Now().UnixNano())
	if len(ns) > 2*n {
		return ns[len(ns)-2*n:]
	}
	return ns
}

// LoadRecord parses a single audit record file. Callers who want to walk a
// directory should use ListRecords to collect the paths first.
func LoadRecord(path string) (AuditRecord, error) {
	var rec AuditRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, fmt.Errorf("audit: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("audit: parse %s: %w", path, err)
	}
	return rec, nil
}

// ListRecords returns every `*.json` file directly under dir, sorted by
// name (which is also chronological given our naming scheme). Missing dirs
// produce nil, nil — "no records yet" is a normal state, not an error.
func ListRecords(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: list %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}
