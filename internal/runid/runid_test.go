package runid

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/receipt"
)

func mustNew(t *testing.T) RunID {
	t.Helper()
	id, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return id
}

func TestNewGeneratesValidUniqueIDs(t *testing.T) {
	seen := make(map[RunID]bool, 256)
	for i := 0; i < 256; i++ {
		id := mustNew(t)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id.String(), idPrefix) {
			t.Fatalf("id %q lacks prefix %q", id, idPrefix)
		}
		if _, err := Parse(id.String()); err != nil {
			t.Fatalf("generated id failed Parse: %v", err)
		}
	}
}

// TestGeneratedIDIsReceiptWritable: the point of a run id is to land in a
// receipt. A generated id must satisfy the frozen ledger schema.
func TestGeneratedIDIsReceiptWritable(t *testing.T) {
	id := mustNew(t)
	if !receipt.ValidIdentifier(id.String()) {
		t.Fatalf("generated id %q is not a valid receipt identifier", id)
	}

	started := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	r := receipt.Record{
		SchemaVersion: receipt.SchemaVersion,
		RecordKind:    receipt.KindRunReceipt,
		ReceiptID:     "rcpt-runid",
		TaskID:        "task-runid",
		RunID:         id.String(),
		StartedAt:     &started,
		FinishedAt:    &finished,
		Repo: &receipt.Repo{
			Identity:        "github.com/Gere2/neurofs",
			BaseCommit:      "abc123",
			InitialTreeHash: strings.Repeat("a", 64),
			FinalTreeHash:   strings.Repeat("a", 64),
		},
		Policy: &receipt.Policy{
			PolicyHash:          strings.Repeat("b", 64),
			Enforcement:         receipt.EnforcementLocal,
			EligibleCatalogHash: strings.Repeat("c", 64),
			Decision:            receipt.DecisionDeny,
		},
		Verification: &receipt.Verification{
			TaskSpecHash:   strings.Repeat("d", 64),
			Classification: receipt.ClassificationInconclusive,
		},
		HumanOutcome: receipt.HumanUnreviewed,
	}
	if err := r.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("receipt carrying generated run id rejected: %v", err)
	}
}

func TestParseRejections(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "empty"},
		{"space padded", " run-abc ", "whitespace"},
		{"leading space", " run-abc", "whitespace"},
		{"trailing tab", "run-abc\t", "whitespace"},
		{"newline", "run-abc\nrun-def", "one line"},
		{"carriage return", "run-abc\r", "one line"},
		{"space inside", "run abc", "must match"},
		{"leading dash", "-run-abc", "must match"},
		{"leading dot", ".run", "must match"},
		{"slash", "run/abc", "must match"},
		{"equals sign", "run=abc", "must match"},
		{"missing prefix", "job-abc", "must start"},
		{"too long", "run-" + strings.Repeat("a", 125), "must match"},
		{"no prefix", "abc123", "must start with"},
		{"wrong prefix", "session-abc", "must start with"},
		{"prefix not at start", "x-run-abc", "must start with"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}

	// 128 chars is the documented ceiling and must be accepted.
	if _, err := Parse("run-" + strings.Repeat("a", 124)); err != nil {
		t.Fatalf("128-char id rejected: %v", err)
	}
}

func TestInjectEnv(t *testing.T) {
	id := mustNew(t)
	want := EnvVar + "=" + id.String()

	t.Run("appends to clean env", func(t *testing.T) {
		out, err := InjectEnv([]string{"PATH=/bin", "HOME=/root"}, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 3 || out[2] != want {
			t.Fatalf("got %v", out)
		}
	})

	t.Run("replaces existing entries", func(t *testing.T) {
		base := []string{
			"PATH=/bin",
			EnvVar + "=run-stale",
			"HOME=/root",
			EnvVar + "=run-staler",
		}
		out, err := InjectEnv(base, id)
		if err != nil {
			t.Fatal(err)
		}
		var hits int
		for _, e := range out {
			if strings.EqualFold(envKey(e), EnvVar) {
				hits++
				if e != want {
					t.Fatalf("stale entry survived: %q", e)
				}
			}
		}
		if hits != 1 {
			t.Fatalf("want exactly one run-id entry, got %d in %v", hits, out)
		}
		if len(out) != 3 {
			t.Fatalf("other vars lost or duplicated: %v", out)
		}
	})

	t.Run("removes case variants and bare names", func(t *testing.T) {
		base := []string{"neurofs_run_id=run-lower", "Neurofs_Run_Id=run-mixed", EnvVar}
		out, err := InjectEnv(base, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0] != want {
			t.Fatalf("got %v", out)
		}
	})

	t.Run("does not mutate base", func(t *testing.T) {
		base := []string{"PATH=/bin", EnvVar + "=run-stale"}
		snapshot := append([]string{}, base...)
		if _, err := InjectEnv(base, id); err != nil {
			t.Fatal(err)
		}
		for i := range base {
			if base[i] != snapshot[i] {
				t.Fatalf("base mutated at %d: %q != %q", i, base[i], snapshot[i])
			}
		}
	})

	t.Run("rejects zero and invalid ids", func(t *testing.T) {
		if _, err := InjectEnv(nil, ""); err == nil {
			t.Fatal("zero id must be rejected")
		}
		if _, err := InjectEnv(nil, RunID("run abc")); err == nil {
			t.Fatal("invalid id must be rejected")
		}
	})
}

// TestInjectEnvDoesNotTouchProcessEnv: propagation is per-child by contract.
func TestInjectEnvDoesNotTouchProcessEnv(t *testing.T) {
	before := os.Environ()
	if _, err := InjectEnv(before, mustNew(t)); err != nil {
		t.Fatal(err)
	}
	after := os.Environ()
	if len(before) != len(after) {
		t.Fatalf("process environment changed: %d -> %d entries", len(before), len(after))
	}
	if _, ok := os.LookupEnv(EnvVar); ok {
		if _, wasSet := lookupInSlice(before, EnvVar); !wasSet {
			t.Fatalf("%s appeared in the process environment", EnvVar)
		}
	}
}

// TestNoProcessEnvMutation guards the "never mutate the process environment"
// contract against future edits: the rule is only worth stating if something
// enforces it. The check is on call expressions, not source text — the package
// documentation names these functions on purpose.
func TestNoProcessEnvMutation(t *testing.T) {
	banned := map[string]bool{"Setenv": true, "Unsetenv": true, "Clearenv": true}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			files++
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "os" || !banned[sel.Sel.Name] {
					return true
				}
				t.Errorf("%s:%d calls os.%s: the run identity must never be made ambient",
					path, fset.Position(call.Pos()).Line, sel.Sel.Name)
				return true
			})
		}
	}
	if files == 0 {
		t.Fatal("no source files parsed")
	}
}

func lookupInSlice(env []string, key string) (string, bool) {
	for _, e := range env {
		if envKey(e) == key {
			if i := strings.IndexByte(e, '='); i >= 0 {
				return e[i+1:], true
			}
			return "", true
		}
	}
	return "", false
}

func TestLookupRunID(t *testing.T) {
	fake := func(value string, present bool) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k != EnvVar {
				t.Fatalf("looked up %q, want %q", k, EnvVar)
			}
			return value, present
		}
	}

	t.Run("absent", func(t *testing.T) {
		id, set, err := lookupRunID(fake("", false))
		if set || err != nil || !id.IsZero() {
			t.Fatalf("got %q, %v, %v", id, set, err)
		}
	})

	t.Run("valid", func(t *testing.T) {
		id, set, err := lookupRunID(fake("run-abc", true))
		if !set || err != nil || id != RunID("run-abc") {
			t.Fatalf("got %q, %v, %v", id, set, err)
		}
	})

	// Present-but-malformed is distinct from absent: it means something tried
	// to correlate and got it wrong, which must surface.
	t.Run("malformed is set with error", func(t *testing.T) {
		id, set, err := lookupRunID(fake("run abc", true))
		if !set {
			t.Fatal("malformed value must report set=true")
		}
		if err == nil || !strings.Contains(err.Error(), EnvVar) {
			t.Fatalf("want error naming %s, got %v", EnvVar, err)
		}
		if !id.IsZero() {
			t.Fatalf("malformed value yielded id %q", id)
		}
	})

	t.Run("empty value is malformed, not absent", func(t *testing.T) {
		_, set, err := lookupRunID(fake("", true))
		if !set || err == nil {
			t.Fatalf("empty-but-present must be an error: set=%v err=%v", set, err)
		}
	})
}

func TestResolve(t *testing.T) {
	const ambient = RunID("run-ambient")
	const explicit = "run-explicit"

	t.Run("neither", func(t *testing.T) {
		id, set, err := resolve("", "", false, nil)
		if set || err != nil || !id.IsZero() {
			t.Fatalf("got %q, %v, %v", id, set, err)
		}
	})

	t.Run("ambient only", func(t *testing.T) {
		id, set, err := resolve("", ambient, true, nil)
		if !set || err != nil || id != ambient {
			t.Fatalf("got %q, %v, %v", id, set, err)
		}
	})

	t.Run("explicit only", func(t *testing.T) {
		id, set, err := resolve(explicit, "", false, nil)
		if !set || err != nil || id != RunID(explicit) {
			t.Fatalf("got %q, %v, %v", id, set, err)
		}
	})

	t.Run("both agree", func(t *testing.T) {
		id, set, err := resolve(ambient.String(), ambient, true, nil)
		if !set || err != nil || id != ambient {
			t.Fatalf("got %q, %v, %v", id, set, err)
		}
	})

	t.Run("conflict is an error", func(t *testing.T) {
		_, _, err := resolve(explicit, ambient, true, nil)
		if err == nil {
			t.Fatal("conflicting identities must error")
		}
		msg := err.Error()
		for _, want := range []string{explicit, ambient.String(), "refusing"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q does not mention %q", msg, want)
			}
		}
	})

	t.Run("invalid explicit", func(t *testing.T) {
		if _, _, err := resolve("run abc", "", false, nil); err == nil {
			t.Fatal("invalid explicit id must error")
		}
	})

	t.Run("malformed ambient surfaces even with explicit", func(t *testing.T) {
		ambientErr := lookupErr()
		if _, _, err := resolve(explicit, "", true, ambientErr); err == nil {
			t.Fatal("malformed ambient must not be masked by an explicit id")
		}
		if _, _, err := resolve("", "", true, ambientErr); err == nil {
			t.Fatal("malformed ambient must surface on its own")
		}
	})
}

func lookupErr() error {
	_, _, err := lookupRunID(func(string) (string, bool) { return "bad id", true })
	return err
}

// TestFromEnvIsReadOnce pins the immutability guarantee: the answer is fixed
// at first read and later environment mutation cannot change it. Runs last-ish
// by name ordering is not relied upon — it only asserts stability, whatever
// the first observed value was.
func TestFromEnvIsReadOnce(t *testing.T) {
	first, firstSet, firstErr := FromEnv()

	t.Setenv(EnvVar, "run-mutated-after-first-read")
	second, secondSet, secondErr := FromEnv()

	if first != second || firstSet != secondSet ||
		(firstErr == nil) != (secondErr == nil) {
		t.Fatalf("FromEnv changed after environment mutation: (%q,%v,%v) -> (%q,%v,%v)",
			first, firstSet, firstErr, second, secondSet, secondErr)
	}
}

const (
	subprocessMarker = "NEUROFS_RUNID_TEST_CHILD"
	subprocessExpect = "NEUROFS_RUNID_TEST_EXPECT"
)

// TestEnvPropagationAcrossProcess is the end-to-end proof of the whole
// mechanism: InjectEnv builds a child environment exactly as an adapter would
// for exec.Cmd.Env, and the child — a real separate process — recovers the
// identity through FromEnv. Everything else in this package tests one side of
// that boundary; this crosses it.
func TestEnvPropagationAcrossProcess(t *testing.T) {
	if os.Getenv(subprocessMarker) == "1" {
		childAssertions(t)
		return
	}

	id := mustNew(t)
	parentEnv := os.Environ()

	injected, err := InjectEnv(parentEnv, id)
	if err != nil {
		t.Fatal(err)
	}

	// A malformed ambient value cannot be produced by InjectEnv (it validates),
	// so it is planted directly — this is the "someone else set it wrong" case.
	malformed := append(append([]string{}, parentEnv...), EnvVar+"=bad id")

	cases := []struct {
		name   string
		env    []string
		expect string
	}{
		{"valid ambient id", injected, id.String()},
		{"absent", parentEnv, "unavailable"},
		{"malformed ambient id", malformed, "unavailable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestEnvPropagationAcrossProcess$", "-test.v")
			cmd.Env = append(append([]string{}, tc.env...),
				subprocessMarker+"=1",
				subprocessExpect+"="+tc.expect)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child failed: %v\n%s", err, out)
			}
		})
	}
}

func childAssertions(t *testing.T) {
	expect := os.Getenv(subprocessExpect)
	a := ForOwnedProcessTree()

	if err := a.Validate(); err != nil {
		t.Fatalf("child produced an invalid availability %+v: %v", a, err)
	}

	if expect == "unavailable" {
		if a.Available() {
			t.Fatalf("child correlated when it should not have: %+v", a)
		}
		if a.Reason == "" {
			t.Fatal("child reported unavailable without a reason")
		}
		return
	}

	if !a.Available() {
		t.Fatalf("child lost the injected identity: %+v", a)
	}
	if a.RunID.String() != expect {
		t.Fatalf("child saw %q, want %q", a.RunID, expect)
	}
	// The environment is the child's only source, so Current must agree.
	current, err := Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.RunID != a.RunID {
		t.Fatalf("Current disagrees with ForOwnedProcessTree: %q vs %q", current.RunID, a.RunID)
	}
	// Resolve must accept the ambient id and reject a conflicting explicit one.
	if _, _, err := Resolve(expect); err != nil {
		t.Fatalf("Resolve rejected the agreeing explicit id: %v", err)
	}
	if _, _, err := Resolve("run-something-else"); err == nil {
		t.Fatal("Resolve accepted a conflicting explicit id")
	}
}

func TestContextPropagation(t *testing.T) {
	id := mustNew(t)

	t.Run("absent by default", func(t *testing.T) {
		if got, ok := FromContext(context.Background()); ok {
			t.Fatalf("empty context carried %q", got)
		}
	})

	t.Run("nil parent context is rejected", func(t *testing.T) {
		//nolint:staticcheck // deliberately exercising the nil guard
		if _, err := NewContext(nil, id); err == nil {
			t.Fatal("nil parent context must be rejected")
		}
	})

	t.Run("nil context is safe", func(t *testing.T) {
		//nolint:staticcheck // deliberately exercising the nil guard
		if _, ok := FromContext(nil); ok {
			t.Fatal("nil context reported an identity")
		}
	})

	t.Run("round trip", func(t *testing.T) {
		ctx, err := NewContext(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := FromContext(ctx)
		if !ok || got != id {
			t.Fatalf("got %q, %v", got, ok)
		}
	})

	t.Run("survives derived contexts", func(t *testing.T) {
		ctx, err := NewContext(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		derived, cancel := context.WithCancel(ctx)
		defer cancel()
		if got, ok := FromContext(derived); !ok || got != id {
			t.Fatalf("identity lost in derived context: %q, %v", got, ok)
		}
	})

	t.Run("rebinding same id is idempotent", func(t *testing.T) {
		ctx, err := NewContext(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		again, err := NewContext(ctx, id)
		if err != nil {
			t.Fatalf("idempotent rebind failed: %v", err)
		}
		if got, _ := FromContext(again); got != id {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("rebinding a different id is an error", func(t *testing.T) {
		ctx, err := NewContext(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		other := mustNew(t)
		if _, err := NewContext(ctx, other); err == nil ||
			!strings.Contains(err.Error(), "refusing to rebind") {
			t.Fatalf("want rebind refusal, got %v", err)
		}
	})

	t.Run("rejects zero and invalid ids", func(t *testing.T) {
		if _, err := NewContext(context.Background(), ""); err == nil {
			t.Fatal("zero id must be rejected")
		}
		if _, err := NewContext(context.Background(), RunID("run abc")); err == nil {
			t.Fatal("invalid id must be rejected")
		}
	})
}

func TestForPersistentServerNeverCorrelates(t *testing.T) {
	// Even with a perfectly good ambient identity, a shared server must not
	// claim it: the value describes the launch, not the current request.
	t.Setenv(EnvVar, "run-from-launch-environment")

	a := ForPersistentServer()
	if a.Available() {
		t.Fatal("persistent server reported correlation as available")
	}
	if a.Correlation != CorrelationUnavailable {
		t.Fatalf("got correlation %q", a.Correlation)
	}
	if !a.RunID.IsZero() {
		t.Fatalf("persistent server attached id %q", a.RunID)
	}
	if !strings.Contains(a.Reason, "request-scoped") {
		t.Fatalf("reason does not name the missing mechanism: %q", a.Reason)
	}
}

func TestForOwnedProcessTree(t *testing.T) {
	// FromEnv is read-once per process, so this reflects whatever the ambient
	// environment was at first read. Assert the invariant that holds either
	// way: availability and the presence of an id agree, and an unavailable
	// result always explains itself.
	a := ForOwnedProcessTree()
	if a.Available() != !a.RunID.IsZero() {
		t.Fatalf("availability %v disagrees with id %q", a.Available(), a.RunID)
	}
	if a.Available() {
		if a.Correlation != CorrelationOwnedProcessTree {
			t.Fatalf("got correlation %q", a.Correlation)
		}
		if _, err := Parse(a.RunID.String()); err != nil {
			t.Fatalf("available correlation carries invalid id: %v", err)
		}
		return
	}
	if a.Correlation != CorrelationUnavailable {
		t.Fatalf("got correlation %q", a.Correlation)
	}
	if a.Reason == "" {
		t.Fatal("unavailable correlation must explain itself")
	}
}

func TestAvailabilityRejectsFabricatedRunID(t *testing.T) {
	a := Availability{RunID: "bad id", Correlation: CorrelationOwnedProcessTree}
	if a.Available() {
		t.Fatal("invalid directly-constructed RunID reported available")
	}
	if err := a.Validate(); err == nil {
		t.Fatal("invalid directly-constructed RunID passed validation")
	}
}

func TestPersistentAvailabilityInContextOverridesAmbientLookup(t *testing.T) {
	ctx, err := WithAvailability(context.Background(), ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}
	a, err := Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Available() || a.Correlation != CorrelationUnavailable {
		t.Fatalf("got %+v", a)
	}
}

func TestJoinKeyValidate(t *testing.T) {
	id := mustNew(t)
	full := JoinKey{RunID: id, BundlePath: ".neurofs/task/x.bundle.json", BundleHash: strings.Repeat("a", 64)}
	if err := full.Validate(); err != nil {
		t.Fatalf("complete key rejected: %v", err)
	}

	cases := []struct {
		name string
		key  JoinKey
		want string
	}{
		{"no run id", JoinKey{BundlePath: full.BundlePath, BundleHash: full.BundleHash}, "run_id required"},
		{"invalid run id", JoinKey{RunID: "run abc", BundlePath: full.BundlePath, BundleHash: full.BundleHash}, "must match"},
		{"no bundle path", JoinKey{RunID: id, BundleHash: full.BundleHash}, "newest bundle"},
		{"no bundle hash", JoinKey{RunID: id, BundlePath: full.BundlePath}, "does not pin content"},
		{"escaping path", JoinKey{RunID: id, BundlePath: "../../etc/passwd", BundleHash: full.BundleHash}, "repo-relative"},
		{"absolute path", JoinKey{RunID: id, BundlePath: "/tmp/bundle.json", BundleHash: full.BundleHash}, "repo-relative"},
		{"windows path", JoinKey{RunID: id, BundlePath: `C:\\tmp\\bundle.json`, BundleHash: full.BundleHash}, "repo-relative"},
		{"short hash", JoinKey{RunID: id, BundlePath: full.BundlePath, BundleHash: "abc"}, "sha256"},
		{"uppercase hash", JoinKey{RunID: id, BundlePath: full.BundlePath, BundleHash: strings.Repeat("A", 64)}, "sha256"},
		{"empty", JoinKey{}, "run_id required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.key.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}
