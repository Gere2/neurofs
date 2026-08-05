package receipt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "regenerate golden testdata")

const goldenPath = "testdata/run_receipts_v1_golden.jsonl"

func fakeSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func intp(i int) *int { return &i }

func ts(hour, min, sec int) *time.Time {
	t := time.Date(2026, 7, 29, hour, min, sec, 0, time.UTC)
	return &t
}

func mustSeal(t *testing.T, r *Record) {
	t.Helper()
	if err := r.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
}

// validReceipt builds a fully populated, sealed run_receipt exercising every
// field of the v1 schema, including multiple model observations and an
// explicitly unknown usage figure.
func validReceipt(t *testing.T) Record {
	t.Helper()
	verifier := VerifierCommand{
		Argv:      []string{"go", "test", "./internal/cli/", "-run", "TestPackChunks"},
		Cwd:       "/work/checkouts/run-0001",
		TimeoutMS: 120000,
	}
	r := Record{
		SchemaVersion: SchemaVersion,
		RecordKind:    KindRunReceipt,
		ReceiptID:     "rcpt-0001",
		TaskID:        "task-testfix-pack-chunks",
		RunID:         "run-0001",
		StartedAt:     ts(10, 0, 0),
		FinishedAt:    ts(10, 2, 30),
		Repo: &Repo{
			Identity:        "github.com/Gere2/neurofs",
			BaseCommit:      "2d347b0c1f4a9e8b7d6c5a4f3e2d1c0b9a8f7e6d",
			InitialTreeHash: fakeSHA("tree-initial"),
			FinalTreeHash:   fakeSHA("tree-final"),
		},
		Policy: &Policy{
			PolicyHash:          fakeSHA("policy-v1"),
			Enforcement:         EnforcementProvider,
			EligibleCatalogHash: fakeSHA("catalog-after-policy"),
			Decision:            DecisionAllow,
		},
		Attempts: []Attempt{{
			AttemptID:     "att-0001",
			Surface:       "copilot_cli",
			SelectionMode: SelectionAuto,
			Argv:          []string{"copilot", "-p", "fix failing test TestPackChunks", "--allow-tool", "shell"},
			Exit:          intp(0),
			DurationMS:    84000,
			ModelObservations: []ModelObservation{
				{Model: "claude-sonnet-5", Source: "cli_stats_footer", ObservedAt: ts(10, 1, 10)},
				{Model: "gpt-5.4-mini", Source: "cli_stats_footer", ObservedAt: ts(10, 2, 5)},
			},
		}},
		Context: []ContextBundle{{
			BundlePath: ".neurofs/task/20260729-100010.bundle.json",
			BundleHash: fakeSHA("bundle-content"),
			Tokens:     intp(2874),
			Provenance: ProvenanceObserved,
		}},
		Usage: []Usage{
			{Metric: "prompt_tokens", Quantity: "2874", Unit: "tokens", Provenance: ProvenanceObserved, Confidence: ConfidenceMedium},
			{Metric: "premium_requests", Unit: "requests", Provenance: ProvenanceUnknown, Confidence: ConfidenceUnknown},
		},
		Verification: &Verification{
			TaskSpecHash: fakeSHA("task-spec"),
			Baseline: &VerifierRun{
				Command: verifier, Exit: intp(1),
				TestsRun: 1, TestsFailed: 1, DurationMS: 2300,
				OutputSHA256: fakeSHA("baseline-output"),
			},
			Final: &VerifierRun{
				Command: verifier, Exit: intp(0),
				TestsRun: 1, DurationMS: 2100,
				OutputSHA256: fakeSHA("final-output"),
			},
			Integrity: &Integrity{
				VerifierSurfacePreSHA256:  fakeSHA("verifier-surface"),
				VerifierSurfacePostSHA256: fakeSHA("verifier-surface"),
			},
			Classification: ClassificationCleanPass,
		},
		Artifacts: []Artifact{
			{Kind: "diff", Path: "audit/runs/run-0001/changes.diff", SHA256: fakeSHA("diff")},
			{Kind: "agent_output", Path: "audit/runs/run-0001/agent_output.txt", SHA256: fakeSHA("agent-output")},
		},
		HumanOutcome: HumanUnreviewed,
	}
	mustSeal(t, &r)
	return r
}

func validAmendment(t *testing.T) Record {
	t.Helper()
	r := Record{
		SchemaVersion:     SchemaVersion,
		RecordKind:        KindRunAmendment,
		ReceiptID:         "rcpt-0002",
		CorrectsReceiptID: "rcpt-0001",
		CreatedAt:         ts(15, 30, 0),
		HumanOutcome:      HumanAccepted,
		Usage: []Usage{
			{Metric: "premium_requests", Quantity: "1", Unit: "requests", Provenance: ProvenanceProviderReported, Confidence: ConfidenceHigh},
		},
		Note: "billing dashboard confirmed 1 premium request; diff accepted after review",
	}
	mustSeal(t, &r)
	return r
}

func TestGolden(t *testing.T) {
	records := []Record{validReceipt(t), validAmendment(t)}

	if *update {
		var buf bytes.Buffer
		for _, r := range records {
			line, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("marshal golden: %v", err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to regenerate): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != len(records) {
		t.Fatalf("golden has %d lines, want %d", len(lines), len(records))
	}

	// The builders (current schema) must reproduce the stored bytes exactly;
	// this is what fails loudly when the schema drifts without -update.
	for i, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal builder %d: %v", i, err)
		}
		if string(line) != lines[i] {
			t.Errorf("builder %d differs from stored golden line\nbuilder: %s\nstored:  %s", i, line, lines[i])
		}
	}

	var parsed []Record
	for i, line := range lines {
		r, err := DecodeRecord([]byte(line))
		if err != nil {
			t.Fatalf("line %d: decode: %v", i, err)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("line %d: validate: %v", i, err)
		}
		if err := r.VerifyContentSHA256(); err != nil {
			t.Errorf("line %d: verify hash: %v", i, err)
		}
		// The stored encoding must round-trip byte-for-byte: golden bytes are
		// the hash-contract encoding.
		remarshaled, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("line %d: remarshal: %v", i, err)
		}
		if string(remarshaled) != line {
			t.Errorf("line %d: stored encoding differs from round-trip encoding\nstored:  %s\nremarsh: %s", i, line, remarshaled)
		}
		parsed = append(parsed, r)
	}
	if err := ValidateSet(parsed); err != nil {
		t.Errorf("validate set: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, r := range []Record{validReceipt(t), validAmendment(t)} {
		b1, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var back Record
		if err := json.Unmarshal(b1, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		b2, err := json.Marshal(back)
		if err != nil {
			t.Fatalf("remarshal: %v", err)
		}
		if !bytes.Equal(b1, b2) {
			t.Errorf("round-trip changed encoding:\n%s\n%s", b1, b2)
		}
		if err := back.VerifyContentSHA256(); err != nil {
			t.Errorf("hash lost in round-trip: %v", err)
		}
	}
}

// TestMinimalReceipt: every optional block absent must still be valid — a
// denied run has no attempts, bundles, usage or artifacts.
func TestMinimalReceipt(t *testing.T) {
	r := Record{
		SchemaVersion: SchemaVersion,
		RecordKind:    KindRunReceipt,
		ReceiptID:     "rcpt-min",
		TaskID:        "task-min",
		RunID:         "run-min",
		StartedAt:     ts(11, 0, 0),
		FinishedAt:    ts(11, 0, 1),
		Repo: &Repo{
			Identity:        "github.com/Gere2/neurofs",
			BaseCommit:      "abc123",
			InitialTreeHash: fakeSHA("t"),
			FinalTreeHash:   fakeSHA("t"),
		},
		Policy: &Policy{
			PolicyHash:          fakeSHA("p"),
			Enforcement:         EnforcementLocal,
			EligibleCatalogHash: fakeSHA("c"),
			Decision:            DecisionDeny,
		},
		Verification: &Verification{
			TaskSpecHash:   fakeSHA("spec"),
			Classification: ClassificationInconclusive,
		},
		HumanOutcome: HumanUnreviewed,
	}
	mustSeal(t, &r)
	if err := r.Validate(); err != nil {
		t.Fatalf("minimal receipt should validate: %v", err)
	}
}

// TestUnknownFieldRejected: strict decoding is part of the integrity story —
// a tolerated unknown field would be invisible to the content hash. Forward
// compatibility is governed by schema_version, not by ignoring fields.
func TestUnknownFieldRejected(t *testing.T) {
	r := validReceipt(t)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	patched := bytes.Replace(b, []byte(`{"schema_version"`), []byte(`{"future_field":true,"schema_version"`), 1)
	if _, err := DecodeRecord(patched); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown-field rejection, got %v", err)
	}

	trailing := append(append([]byte{}, b...), []byte(`{"schema_version":1}`)...)
	if _, err := DecodeRecord(trailing); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("want trailing-data rejection, got %v", err)
	}
}

// TestDecodeRejectsNonCanonical: every representation the content hash cannot
// see must fail the decode, not slip through as an equivalent record.
func TestDecodeRejectsNonCanonical(t *testing.T) {
	r := validReceipt(t)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRecord(b); err != nil {
		t.Fatalf("canonical line must decode: %v", err)
	}

	cases := []struct {
		name    string
		input   []byte
		wantSub string
	}{
		{"duplicate known key (last wins)",
			bytes.Replace(b, []byte(`{"schema_version":1`), []byte(`{"schema_version":2,"schema_version":1`), 1),
			"canonical"},
		{"case-variant key (case-insensitive match)",
			bytes.Replace(b, []byte(`"task_id"`), []byte(`"Task_ID"`), 1),
			"canonical"},
		{"null overwriting a typed field",
			bytes.Replace(b, []byte(`"task_id":"task-testfix-pack-chunks"`), []byte(`"task_id":null`), 1),
			"canonical"},
		{"whitespace variant",
			bytes.Replace(b, []byte(`"record_kind":"run_receipt"`), []byte(`"record_kind": "run_receipt"`), 1),
			"canonical"},
		{"trailing close bracket (Decoder.More blind spot)",
			append(append([]byte{}, b...), ']'),
			"trailing"},
		{"leading whitespace",
			append([]byte(" "), b...),
			"canonical"},
		{"trailing whitespace",
			append(append([]byte{}, b...), ' '),
			"canonical"},
		{"leading and trailing whitespace",
			append(append([]byte(" "), b...), ' '),
			"canonical"},
		{"trailing newline inside the line",
			append(append([]byte{}, b...), '\n'),
			"canonical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Equal(tc.input, b) {
				t.Fatal("test setup: mutation did not apply")
			}
			_, err := DecodeRecord(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

func TestReceiptValidationRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Record)
		wantSub string
	}{
		{"wrong schema version", func(r *Record) { r.SchemaVersion = 2 }, "schema_version"},
		{"unknown record kind", func(r *Record) { r.RecordKind = "receipt" }, "record_kind"},
		{"receipt id with space", func(r *Record) { r.ReceiptID = "rcpt 1" }, "receipt_id"},
		{"missing run id", func(r *Record) { r.RunID = "" }, "run_id"},
		{"finished before started", func(r *Record) { r.FinishedAt = ts(9, 0, 0) }, "precedes"},
		{"missing repo", func(r *Record) { r.Repo = nil }, "repo is required"},
		{"empty tree hash", func(r *Record) { r.Repo.InitialTreeHash = "" }, "initial_tree_hash"},
		{"bad enforcement", func(r *Record) { r.Policy.Enforcement = "trusted" }, "enforcement"},
		{"bad policy hash", func(r *Record) { r.Policy.PolicyHash = "not-hex" }, "policy_hash"},
		{"bad decision", func(r *Record) { r.Policy.Decision = "maybe" }, "decision"},
		{"deny with attempts", func(r *Record) { r.Policy.Decision = DecisionDeny }, "deny forbids attempts"},
		{"duplicate attempt id", func(r *Record) {
			r.Attempts = append(r.Attempts, r.Attempts[0])
			r.Verification.Classification = ClassificationFailed // avoid tripping the 1-attempt clean rule first
		}, "duplicate"},
		{"empty argv", func(r *Record) { r.Attempts[0].Argv = nil }, "argv"},
		{"missing selection_mode", func(r *Record) { r.Attempts[0].SelectionMode = "" }, "selection_mode"},
		{"bad selection_mode", func(r *Record) { r.Attempts[0].SelectionMode = "manual" }, "selection_mode"},
		{"explicit without requested_model", func(r *Record) { r.Attempts[0].SelectionMode = SelectionExplicit }, "requested_model"},
		{"auto with requested_model", func(r *Record) { r.Attempts[0].RequestedModel = "claude-sonnet-5" }, "must be empty"},
		{"negative tokens", func(r *Record) { r.Context[0].Tokens = intp(-1) }, "tokens"},
		{"tokens with unknown provenance", func(r *Record) { r.Context[0].Provenance = ProvenanceUnknown }, "absence never means zero"},
		{"missing tokens with observed provenance", func(r *Record) { r.Context[0].Tokens = nil }, "required"},
		{"bad bundle provenance", func(r *Record) { r.Context[0].Provenance = "guessed" }, "provenance"},
		{"absolute bundle path", func(r *Record) { r.Context[0].BundlePath = "/etc/passwd" }, "relative"},
		{"quantity with unknown provenance", func(r *Record) { r.Usage[1].Quantity = "0" }, "absence never means zero"},
		{"observed without quantity", func(r *Record) { r.Usage[0].Quantity = "" }, "decimal"},
		{"float exponent quantity", func(r *Record) { r.Usage[0].Quantity = "1e6" }, "decimal"},
		{"negative quantity", func(r *Record) { r.Usage[0].Quantity = "-3" }, "decimal"},
		{"bad confidence", func(r *Record) { r.Usage[0].Confidence = "sure" }, "confidence"},
		{"missing verification", func(r *Record) { r.Verification = nil }, "verification is required"},
		{"bad classification", func(r *Record) { r.Verification.Classification = "passed" }, "classification"},
		{"artifact path escape", func(r *Record) { r.Artifacts[0].Path = "../outside.diff" }, "escapes"},
		{"artifact backslash path", func(r *Record) { r.Artifacts[0].Path = `audit\runs\x.diff` }, "relative"},
		{"bad artifact hash", func(r *Record) { r.Artifacts[0].SHA256 = "abc" }, "sha256"},
		{"bad human outcome", func(r *Record) { r.HumanOutcome = "fine" }, "human_outcome"},
		{"corrects on receipt", func(r *Record) { r.CorrectsReceiptID = "rcpt-0000" }, "amendment-only"},
		{"note on receipt", func(r *Record) { r.Note = "looks good" }, "amendment-only"},
		{"top-level classification on receipt", func(r *Record) { r.Classification = ClassificationFailed }, "amendment-only"},
		{"tests_failed exceeds tests_run", func(r *Record) { r.Verification.Baseline.TestsFailed = 5 }, "exceeds"},
		{"verifier without timeout", func(r *Record) { r.Verification.Final.Command.TimeoutMS = 0 }, "timeout"},

		// clean_pass_at_1 cross-rules.
		{"clean with two attempts", func(r *Record) {
			extra := r.Attempts[0]
			extra.AttemptID = "att-0002"
			r.Attempts = append(r.Attempts, extra)
		}, "exactly one attempt"},
		{"clean without baseline", func(r *Record) { r.Verification.Baseline = nil }, "required"},
		{"clean baseline not reproducing", func(r *Record) { r.Verification.Baseline.TestsFailed = 0 }, "reproduce"},
		{"clean baseline exit zero", func(r *Record) { r.Verification.Baseline.Exit = intp(0) }, "non-zero"},
		{"clean baseline never exited", func(r *Record) { r.Verification.Baseline.Exit = nil }, "non-zero"},
		{"clean final nonzero exit", func(r *Record) { r.Verification.Final.Exit = intp(1) }, "exit 0"},
		{"clean final never exited", func(r *Record) { r.Verification.Final.Exit = nil }, "exit 0"},
		{"clean zero executed tests", func(r *Record) { r.Verification.Final.TestsRun = 0 }, "zero executed tests"},
		{"clean verifier surface changed", func(r *Record) {
			r.Verification.Integrity.VerifierSurfacePostSHA256 = fakeSHA("tampered")
		}, "pass_needs_review"},
		{"clean touched verifier paths", func(r *Record) {
			r.Verification.Integrity.TouchedVerifierPaths = []string{"internal/cli/pack_chunks_test.go"}
		}, "pass_needs_review"},
		{"clean commands differ", func(r *Record) {
			r.Verification.Final.Command.Argv = []string{"go", "test", "./..."}
		}, "commands differ"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validReceipt(t)
			tc.mutate(&r)
			mustSeal(t, &r) // keep the hash well-formed so only the mutation trips validation
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestAmendmentValidation(t *testing.T) {
	base := validAmendment(t)
	if err := base.Validate(); err != nil {
		t.Fatalf("valid amendment rejected: %v", err)
	}

	// Reclassifying downward (e.g. a human discovers the fix was wrong) is
	// legitimate; only clean_pass_at_1 is unreachable by amendment.
	demote := validAmendment(t)
	demote.Classification = ClassificationFailed
	mustSeal(t, &demote)
	if err := demote.Validate(); err != nil {
		t.Fatalf("demoting reclassification rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*Record)
		wantSub string
	}{
		{"missing corrects", func(r *Record) { r.CorrectsReceiptID = "" }, "corrects_receipt_id"},
		{"corrects itself", func(r *Record) { r.CorrectsReceiptID = r.ReceiptID }, "itself"},
		{"missing created_at", func(r *Record) { r.CreatedAt = nil }, "created_at"},
		{"no correction payload", func(r *Record) {
			r.Usage = nil
			r.HumanOutcome = ""
			r.Note = ""
		}, "at least one correction"},
		{"repo on amendment", func(r *Record) { r.Repo = &Repo{} }, "receipt-only"},
		{"run_id on amendment", func(r *Record) { r.RunID = "run-0001" }, "receipt-only"},
		{"verification on amendment", func(r *Record) { r.Verification = &Verification{} }, "receipt-only"},
		{"bad reclassification", func(r *Record) { r.Classification = "better" }, "classification"},
		{"reclassify to clean_pass_at_1", func(r *Record) { r.Classification = ClassificationCleanPass }, "human_outcome accepted"},
		{"bad usage on amendment", func(r *Record) { r.Usage[0].Quantity = "1.5e3" }, "decimal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validAmendment(t)
			tc.mutate(&r)
			mustSeal(t, &r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateSet(t *testing.T) {
	receipt := validReceipt(t)
	amendment := validAmendment(t)

	if err := ValidateSet([]Record{receipt, amendment}); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}

	t.Run("duplicate receipt_id", func(t *testing.T) {
		if err := ValidateSet([]Record{receipt, receipt}); err == nil ||
			!strings.Contains(err.Error(), "duplicate receipt_id") {
			t.Fatalf("want duplicate receipt_id error, got %v", err)
		}
	})

	t.Run("two receipts for one run_id", func(t *testing.T) {
		second := validReceipt(t)
		second.ReceiptID = "rcpt-0003"
		mustSeal(t, &second)
		if err := ValidateSet([]Record{receipt, second}); err == nil ||
			!strings.Contains(err.Error(), "already has receipt") {
			t.Fatalf("want one-receipt-per-run error, got %v", err)
		}
	})

	t.Run("dangling amendment", func(t *testing.T) {
		if err := ValidateSet([]Record{amendment}); err == nil ||
			!strings.Contains(err.Error(), "not found in set") {
			t.Fatalf("want dangling corrects error, got %v", err)
		}
	})

	t.Run("amendment correcting amendment", func(t *testing.T) {
		chained := validAmendment(t)
		chained.ReceiptID = "rcpt-0004"
		chained.CorrectsReceiptID = amendment.ReceiptID
		mustSeal(t, &chained)
		if err := ValidateSet([]Record{receipt, amendment, chained}); err == nil ||
			!strings.Contains(err.Error(), "not a run_receipt") {
			t.Fatalf("want amendment-chain error, got %v", err)
		}
	})
}

func TestHashTamperDetected(t *testing.T) {
	r := validReceipt(t)
	if err := r.VerifyContentSHA256(); err != nil {
		t.Fatalf("sealed record must verify: %v", err)
	}

	tampered := r
	tampered.HumanOutcome = HumanAccepted // an in-place edit, not an amendment
	if err := tampered.VerifyContentSHA256(); err == nil {
		t.Fatal("tampered record passed hash verification")
	}

	var missing Record = r
	missing.ContentSHA256 = ""
	if err := missing.VerifyContentSHA256(); err == nil {
		t.Fatal("record without hash passed verification")
	}
}
