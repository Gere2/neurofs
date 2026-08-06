package receipt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestAppendAndLoad(t *testing.T) {
	repo := t.TempDir()

	r := validReceipt(t)
	r.ContentSHA256 = "" // AppendRecord seals inside the operation
	if err := AppendRecord(repo, &r); err != nil {
		t.Fatalf("append receipt: %v", err)
	}
	if !sha256Re.MatchString(r.ContentSHA256) {
		t.Fatalf("append did not hand the sealed hash back to the caller")
	}

	a := validAmendment(t)
	if err := AppendRecord(repo, &a); err != nil {
		t.Fatalf("append amendment: %v", err)
	}

	records, err := LoadLedger(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].ReceiptID != r.ReceiptID || records[1].ReceiptID != a.ReceiptID {
		t.Fatalf("order not preserved: %s, %s", records[0].ReceiptID, records[1].ReceiptID)
	}

	info, err := os.Stat(LedgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("ledger permissions %o, want 0600", perm)
	}
}

func TestLoadMissingLedgerIsEmpty(t *testing.T) {
	records, err := LoadLedger(t.TempDir())
	if err != nil {
		t.Fatalf("missing ledger must load as empty: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("got %d records from missing ledger", len(records))
	}
}

func TestAppendInvalidRejected(t *testing.T) {
	repo := t.TempDir()
	r := validReceipt(t)
	r.RunID = ""
	if err := AppendRecord(repo, &r); err == nil || !strings.Contains(err.Error(), "run_id") {
		t.Fatalf("want run_id rejection, got %v", err)
	}
	if _, err := os.Stat(LedgerPath(repo)); !os.IsNotExist(err) {
		t.Fatalf("rejected append must not create the ledger, stat err = %v", err)
	}
}

func TestAppendRefusesToPoisonLedger(t *testing.T) {
	repo := t.TempDir()
	first := validReceipt(t)
	if err := AppendRecord(repo, &first); err != nil {
		t.Fatal(err)
	}

	t.Run("duplicate run_id", func(t *testing.T) {
		dup := validReceipt(t)
		dup.ReceiptID = "rcpt-0009"
		if err := AppendRecord(repo, &dup); err == nil || !strings.Contains(err.Error(), "already has receipt") {
			t.Fatalf("want duplicate-run rejection, got %v", err)
		}
	})

	t.Run("dangling amendment", func(t *testing.T) {
		a := validAmendment(t)
		a.ReceiptID = "rcpt-0010"
		a.CorrectsReceiptID = "rcpt-nowhere"
		if err := AppendRecord(repo, &a); err == nil || !strings.Contains(err.Error(), "not found in set") {
			t.Fatalf("want dangling-amendment rejection, got %v", err)
		}
	})

	// The ledger still holds exactly the one good record.
	records, err := LoadLedger(repo)
	if err != nil || len(records) != 1 {
		t.Fatalf("ledger damaged by rejected appends: %d records, err %v", len(records), err)
	}
}

// TestAppendRefusesTamperedHistory: a line edited in place can stay
// structurally valid and canonical while its hash no longer matches; the
// append preflight must refuse to extend such a ledger instead of letting
// LoadLedger discover it later.
func TestAppendRefusesTamperedHistory(t *testing.T) {
	repo := t.TempDir()
	r := validReceipt(t)
	if err := AppendRecord(repo, &r); err != nil {
		t.Fatal(err)
	}

	path := LedgerPath(repo)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"human_outcome":"unreviewed"`), []byte(`"human_outcome":"accepted"`), 1)
	if bytes.Equal(raw, edited) {
		t.Fatal("test setup: edit did not apply")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	a := validAmendment(t)
	err = AppendRecord(repo, &a)
	if err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Fatalf("want refusal to extend tampered ledger, got %v", err)
	}
}

// TestAppendRejectsSymlinkedAuditDir: os.Root confinement — a symlinked
// audit/ directory must not redirect the write outside the repository.
func TestAppendRejectsSymlinkedAuditDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup not portable to windows test environments")
	}
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "audit")); err != nil {
		t.Fatal(err)
	}

	r := validReceipt(t)
	if err := AppendRecord(repo, &r); err == nil {
		t.Fatal("append through symlinked audit/ must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "run_receipts.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ledger escaped the repository, stat err = %v", err)
	}
}

func TestLoadDetectsInPlaceEdit(t *testing.T) {
	repo := t.TempDir()
	r := validReceipt(t)
	if err := AppendRecord(repo, &r); err != nil {
		t.Fatal(err)
	}

	path := LedgerPath(repo)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"human_outcome":"unreviewed"`), []byte(`"human_outcome":"accepted"`), 1)
	if bytes.Equal(raw, edited) {
		t.Fatal("test setup: edit did not apply")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadLedger(repo); err == nil || !strings.Contains(err.Error(), "content_sha256 mismatch") {
		t.Fatalf("want hash mismatch on edited ledger, got %v", err)
	}
}

// TestLedgerFraming: the ledger is a sequence of canonical records each
// terminated by exactly one '\n'. Only a zero-byte file is empty; every
// other shape is malformed and must be surfaced, never silently tolerated
// or skipped.
func TestLedgerFraming(t *testing.T) {
	canonical := func(t *testing.T) []byte {
		t.Helper()
		repo := t.TempDir()
		r := validReceipt(t)
		if err := AppendRecord(repo, &r); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(LedgerPath(repo))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	t.Run("zero-byte file is an empty ledger", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, "audit"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(LedgerPath(repo), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		records, err := LoadLedger(repo)
		if err != nil {
			t.Fatalf("zero-byte ledger must load as empty: %v", err)
		}
		if len(records) != 0 {
			t.Fatalf("got %d records", len(records))
		}
	})

	cases := []struct {
		name    string
		content func(t *testing.T) []byte
		wantSub string
	}{
		{"whitespace-only file is not empty",
			func(t *testing.T) []byte { return []byte("   \n") },
			"blank line"},
		{"newline-only file is not empty",
			func(t *testing.T) []byte { return []byte("\n") },
			"blank line"},
		{"missing final newline is a truncated tail",
			func(t *testing.T) []byte { return bytes.TrimSuffix(canonical(t), []byte("\n")) },
			"truncated"},
		{"blank line between records",
			func(t *testing.T) []byte {
				raw := canonical(t)
				return append(append([]byte{}, raw...), append([]byte("\n"), raw...)...)
			},
			"blank line"},
		{"line padded with spaces",
			func(t *testing.T) []byte {
				raw := bytes.TrimSuffix(canonical(t), []byte("\n"))
				return append(append([]byte(" "), raw...), []byte(" \n")...)
			},
			"canonical"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, "audit"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(LedgerPath(repo), tc.content(t), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadLedger(repo)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
			// A malformed ledger must also refuse further appends.
			a := validAmendment(t)
			if err := AppendRecord(repo, &a); err == nil {
				t.Fatal("append onto a malformed ledger must fail")
			}
		})
	}
}

func TestLoadRejectsUnknownFieldLine(t *testing.T) {
	repo := t.TempDir()
	r := validReceipt(t)
	if err := AppendRecord(repo, &r); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(LedgerPath(repo), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema_version":1,"future_field":true}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if _, err := LoadLedger(repo); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want strict-decode failure, got %v", err)
	}
}

func TestConcurrentAppends(t *testing.T) {
	repo := t.TempDir()
	const n = 12

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := validReceipt(t)
			r.ReceiptID = fmt.Sprintf("rcpt-c%03d", i)
			r.RunID = fmt.Sprintf("run-c%03d", i)
			errs[i] = AppendRecord(repo, &r)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	records, err := LoadLedger(repo)
	if err != nil {
		t.Fatalf("load after concurrent appends: %v", err)
	}
	if len(records) != n {
		t.Fatalf("got %d records, want %d (torn or interleaved writes)", len(records), n)
	}
}
