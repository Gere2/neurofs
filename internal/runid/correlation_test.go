package runid

import (
	"context"
	"strings"
	"testing"
)

const testHash = "3fdba35f04dc8c462986c992bcf875546257113072a909c162f7e470e581e278"

func availOwned(t *testing.T) Availability {
	t.Helper()
	return Availability{RunID: mustNew(t), Correlation: CorrelationOwnedProcessTree}
}

func TestAvailabilityValidate(t *testing.T) {
	owned := availOwned(t)
	if err := owned.Validate(); err != nil {
		t.Fatalf("owned correlation rejected: %v", err)
	}
	unavailable := ForPersistentServer()
	if err := unavailable.Validate(); err != nil {
		t.Fatalf("unavailable correlation rejected: %v", err)
	}

	cases := []struct {
		name string
		a    Availability
		want string
	}{
		{"owned without run id",
			Availability{Correlation: CorrelationOwnedProcessTree},
			"requires run_id"},
		{"owned with invalid run id",
			Availability{RunID: RunID("run abc"), Correlation: CorrelationOwnedProcessTree},
			"must match"},
		{"owned with unprefixed run id",
			Availability{RunID: RunID("abc123"), Correlation: CorrelationOwnedProcessTree},
			"must start with"},
		{"unavailable carrying a run id",
			Availability{RunID: mustNew(t), Correlation: CorrelationUnavailable, Reason: "x"},
			"must not carry run_id"},
		{"unavailable without a reason",
			Availability{Correlation: CorrelationUnavailable},
			"requires a reason"},
		{"unavailable with blank reason",
			Availability{Correlation: CorrelationUnavailable, Reason: "   "},
			"requires a reason"},
		{"empty correlation",
			Availability{},
			"unsupported correlation"},
		{"unknown correlation",
			Availability{Correlation: Correlation("probably")},
			"unsupported correlation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.a.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if tc.a.Available() {
				t.Fatal("an invalid availability must never report Available")
			}
		})
	}
}

func TestWithAvailability(t *testing.T) {
	bg := context.Background()

	t.Run("nil context", func(t *testing.T) {
		//nolint:staticcheck // deliberately exercising the nil guard
		if _, err := WithAvailability(nil, ForPersistentServer()); err == nil {
			t.Fatal("nil context must be rejected")
		}
	})

	t.Run("invalid availability", func(t *testing.T) {
		if _, err := WithAvailability(bg, Availability{Correlation: CorrelationOwnedProcessTree}); err == nil {
			t.Fatal("invalid availability must be rejected")
		}
	})

	t.Run("declares unavailable on a fresh context", func(t *testing.T) {
		ctx, err := WithAvailability(bg, ForPersistentServer())
		if err != nil {
			t.Fatal(err)
		}
		got, ok := AvailabilityFromContext(ctx)
		if !ok || got.Correlation != CorrelationUnavailable || got.Available() {
			t.Fatalf("got %+v, ok=%v", got, ok)
		}
	})

	t.Run("rebinding the same declaration is idempotent", func(t *testing.T) {
		a := ForPersistentServer()
		ctx, err := WithAvailability(bg, a)
		if err != nil {
			t.Fatal(err)
		}
		again, err := WithAvailability(ctx, a)
		if err != nil {
			t.Fatalf("idempotent rebind failed: %v", err)
		}
		if again != ctx {
			t.Fatal("idempotent rebind should return the parent context unchanged")
		}
	})

	t.Run("rebinding a different declaration is an error", func(t *testing.T) {
		ctx, err := WithAvailability(bg, ForPersistentServer())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := WithAvailability(ctx, availOwned(t)); err == nil ||
			!strings.Contains(err.Error(), "refusing to rebind") {
			t.Fatalf("want rebind refusal, got %v", err)
		}
	})

	t.Run("conflicts with an identity-only context", func(t *testing.T) {
		// Reachable only from inside the package: NewContext always sets both
		// keys. Exercising it keeps the defensive branch honest rather than
		// dead.
		ctx := context.WithValue(bg, contextKey{}, mustNew(t))
		if _, err := WithAvailability(ctx, availOwned(t)); err == nil ||
			!strings.Contains(err.Error(), "refusing availability") {
			t.Fatalf("want availability refusal, got %v", err)
		}
	})

	t.Run("agrees with an identity-only context", func(t *testing.T) {
		id := mustNew(t)
		ctx := context.WithValue(bg, contextKey{}, id)
		out, err := WithAvailability(ctx, Availability{RunID: id, Correlation: CorrelationOwnedProcessTree})
		if err != nil {
			t.Fatalf("matching identity rejected: %v", err)
		}
		if got, _ := AvailabilityFromContext(out); got.RunID != id {
			t.Fatalf("got %q", got.RunID)
		}
	})

	t.Run("absent from a bare context", func(t *testing.T) {
		if _, ok := AvailabilityFromContext(bg); ok {
			t.Fatal("bare context declared an availability")
		}
		//nolint:staticcheck // deliberately exercising the nil guard
		if _, ok := AvailabilityFromContext(nil); ok {
			t.Fatal("nil context declared an availability")
		}
	})
}

func TestCurrent(t *testing.T) {
	bg := context.Background()

	t.Run("explicit declaration wins over ambient", func(t *testing.T) {
		// The persistent-server case: even if the launch environment carried an
		// id, the request-scoped declaration must decide.
		ctx, err := WithAvailability(bg, ForPersistentServer())
		if err != nil {
			t.Fatal(err)
		}
		got, err := Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Available() || got.Correlation != CorrelationUnavailable {
			t.Fatalf("got %+v", got)
		}
		if !got.RunID.IsZero() {
			t.Fatalf("unavailable current carried id %q", got.RunID)
		}
	})

	t.Run("run identity from NewContext", func(t *testing.T) {
		id := mustNew(t)
		ctx, err := NewContext(bg, id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Available() || got.RunID != id {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("identity-only context", func(t *testing.T) {
		id := mustNew(t)
		ctx := context.WithValue(bg, contextKey{}, id)
		got, err := Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Available() || got.RunID != id {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("invalid identity-only context surfaces", func(t *testing.T) {
		ctx := context.WithValue(bg, contextKey{}, RunID("bogus"))
		if _, err := Current(ctx); err == nil {
			t.Fatal("an unparseable context identity must error, not be attached")
		}
	})

	t.Run("falls back to the process environment", func(t *testing.T) {
		// FromEnv is read once per process, so assert the invariant that holds
		// either way rather than a fixed outcome.
		got, err := Current(bg)
		if err != nil {
			if _, _, envErr := FromEnv(); envErr == nil {
				t.Fatalf("unexpected error with a well-formed environment: %v", err)
			}
			return
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("Current returned an invalid availability %+v: %v", got, err)
		}
		if !got.Available() && got.Reason == "" {
			t.Fatal("unavailable current must explain itself")
		}
	})
}

func TestBind(t *testing.T) {
	bg := context.Background()
	id := mustNew(t)
	ownedCtx, err := NewContext(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	unavailCtx, err := WithAvailability(bg, ForPersistentServer())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("fills an unlabelled artifact", func(t *testing.T) {
		got, err := Bind(ownedCtx, Availability{})
		if err != nil {
			t.Fatal(err)
		}
		if got.RunID != id || !got.Available() {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("fills an unlabelled artifact as unavailable", func(t *testing.T) {
		got, err := Bind(unavailCtx, Availability{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Available() || got.Reason == "" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("accepts a matching label", func(t *testing.T) {
		existing := Availability{RunID: id, Correlation: CorrelationOwnedProcessTree}
		got, err := Bind(ownedCtx, existing)
		if err != nil {
			t.Fatal(err)
		}
		if got != existing {
			t.Fatalf("got %+v, want %+v", got, existing)
		}
	})

	t.Run("refuses a conflicting label", func(t *testing.T) {
		other := availOwned(t)
		_, err := Bind(ownedCtx, other)
		if err == nil || !strings.Contains(err.Error(), "refusing to overwrite silently") {
			t.Fatalf("want conflict refusal, got %v", err)
		}
	})

	t.Run("refuses relabelling an unavailable context", func(t *testing.T) {
		if _, err := Bind(unavailCtx, availOwned(t)); err == nil {
			t.Fatal("labelling an artifact for a run the context does not have must fail")
		}
	})

	t.Run("refuses an invalid label", func(t *testing.T) {
		invalid := Availability{Correlation: CorrelationOwnedProcessTree}
		if _, err := Bind(ownedCtx, invalid); err == nil {
			t.Fatal("invalid pre-existing label must be rejected")
		}
	})

	t.Run("surfaces a broken context", func(t *testing.T) {
		broken := context.WithValue(bg, contextKey{}, RunID("bogus"))
		if _, err := Bind(broken, Availability{}); err == nil {
			t.Fatal("a broken context must not silently produce an attribution")
		}
	})
}

func TestJoinKeyPathAndHashStrictness(t *testing.T) {
	id := mustNew(t)
	ok := JoinKey{RunID: id, BundlePath: ".neurofs/task/x.bundle.json", BundleHash: testHash}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}

	cases := []struct {
		name string
		key  JoinKey
		want string
	}{
		{"absolute path",
			JoinKey{RunID: id, BundlePath: "/etc/passwd", BundleHash: testHash}, "repo-relative"},
		{"backslash path",
			JoinKey{RunID: id, BundlePath: `audit\x.json`, BundleHash: testHash}, "repo-relative"},
		{"parent escape",
			JoinKey{RunID: id, BundlePath: "../x.json", BundleHash: testHash}, "repo-relative"},
		{"unclean double slash",
			JoinKey{RunID: id, BundlePath: "audit//x.json", BundleHash: testHash}, "repo-relative"},
		{"unclean dot segment",
			JoinKey{RunID: id, BundlePath: "audit/./x.json", BundleHash: testHash}, "repo-relative"},
		{"bare dot",
			JoinKey{RunID: id, BundlePath: ".", BundleHash: testHash}, "repo-relative"},
		{"padded path",
			JoinKey{RunID: id, BundlePath: " audit/x.json ", BundleHash: testHash}, "repo-relative"},
		{"uppercase hash",
			JoinKey{RunID: id, BundlePath: ok.BundlePath, BundleHash: strings.ToUpper(testHash)}, "lowercase sha256"},
		{"short hash",
			JoinKey{RunID: id, BundlePath: ok.BundlePath, BundleHash: testHash[:32]}, "lowercase sha256"},
		{"non-hex hash",
			JoinKey{RunID: id, BundlePath: ok.BundlePath, BundleHash: strings.Repeat("z", 64)}, "lowercase sha256"},
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
