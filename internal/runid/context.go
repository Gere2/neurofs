package runid

import (
	"context"
	"fmt"
)

type contextKey struct{}
type availabilityContextKey struct{}

// NewContext returns a context carrying id.
//
// Propagation is immutable: rebinding a context to a different identity is an
// error, not a shadowing operation. Without that rule a nested call could
// silently relabel the artifacts produced below it, which is the same failure
// mode Resolve refuses at startup. Rebinding to the same identity is
// idempotent and returns the parent unchanged.
func NewContext(ctx context.Context, id RunID) (context.Context, error) {
	if ctx == nil {
		return nil, fmt.Errorf("runid: context: parent context is nil")
	}
	if id.IsZero() {
		return nil, fmt.Errorf("runid: context: no run identity to attach")
	}
	if _, err := Parse(id.String()); err != nil {
		return nil, fmt.Errorf("runid: context: %w", err)
	}
	if existing, ok := FromContext(ctx); ok {
		if existing != id {
			return nil, fmt.Errorf(
				"runid: context already carries run identity %q, refusing to rebind to %q",
				existing, id)
		}
		return ctx, nil
	}
	ctx = context.WithValue(ctx, contextKey{}, id)
	return context.WithValue(ctx, availabilityContextKey{}, Availability{
		RunID:       id,
		Correlation: CorrelationOwnedProcessTree,
	}), nil
}

// FromContext returns the run identity carried by ctx, if any. A false result
// means correlation is unavailable for this call path — callers record that
// fact (see Availability) rather than inventing an id.
func FromContext(ctx context.Context) (RunID, bool) {
	if ctx == nil {
		return "", false
	}
	id, ok := ctx.Value(contextKey{}).(RunID)
	if !ok || id.IsZero() {
		return "", false
	}
	return id, true
}

// WithAvailability declares the correlation topology for all artifact writes
// below ctx. Persistent servers use it to pin correlation to unavailable even
// when their launch environment happens to contain a stale run id.
func WithAvailability(ctx context.Context, availability Availability) (context.Context, error) {
	if ctx == nil {
		return nil, fmt.Errorf("runid: availability context: parent context is nil")
	}
	if err := availability.Validate(); err != nil {
		return nil, err
	}
	if existing, ok := AvailabilityFromContext(ctx); ok {
		if existing.RunID != availability.RunID || existing.Correlation != availability.Correlation {
			return nil, fmt.Errorf(
				"runid: context already declares %q for run %q, refusing to rebind to %q for run %q",
				existing.Correlation, existing.RunID, availability.Correlation, availability.RunID,
			)
		}
		return ctx, nil
	}
	if existing, ok := FromContext(ctx); ok && existing != availability.RunID {
		return nil, fmt.Errorf(
			"runid: context already carries run identity %q, refusing availability for %q",
			existing, availability.RunID,
		)
	}
	return context.WithValue(ctx, availabilityContextKey{}, availability), nil
}

// AvailabilityFromContext returns an explicit topology declaration carried by
// ctx. It never consults the process environment.
func AvailabilityFromContext(ctx context.Context) (Availability, bool) {
	if ctx == nil {
		return Availability{}, false
	}
	availability, ok := ctx.Value(availabilityContextKey{}).(Availability)
	return availability, ok
}

// Current resolves the attribution for an artifact write. An explicit context
// declaration wins because it is request/process scoped. Without one, a
// one-shot CLI may inherit the adapter-provided environment. Malformed ambient
// state is an error; absence is recorded explicitly as unavailable.
func Current(ctx context.Context) (Availability, error) {
	if availability, ok := AvailabilityFromContext(ctx); ok {
		if err := availability.Validate(); err != nil {
			return Availability{}, err
		}
		return availability, nil
	}
	if id, ok := FromContext(ctx); ok {
		availability := Availability{RunID: id, Correlation: CorrelationOwnedProcessTree}
		if err := availability.Validate(); err != nil {
			return Availability{}, err
		}
		return availability, nil
	}
	id, set, err := FromEnv()
	if err != nil {
		return Availability{}, err
	}
	if !set {
		return Availability{
			Correlation: CorrelationUnavailable,
			Reason:      fmt.Sprintf("%s is not set: artifact is not attributable to a NeuroFS-controlled run", EnvVar),
		}, nil
	}
	return Availability{RunID: id, Correlation: CorrelationOwnedProcessTree}, nil
}

// Bind fills an empty artifact attribution from ctx and refuses a pre-labelled
// artifact that disagrees with the current run. That keeps auto-labelling from
// becoming a way to overwrite or forge correlation silently.
func Bind(ctx context.Context, existing Availability) (Availability, error) {
	current, err := Current(ctx)
	if err != nil {
		return Availability{}, err
	}
	if existing == (Availability{}) {
		return current, nil
	}
	if err := existing.Validate(); err != nil {
		return Availability{}, err
	}
	if existing.RunID != current.RunID || existing.Correlation != current.Correlation {
		return Availability{}, fmt.Errorf(
			"runid: artifact attribution %q/%q conflicts with current %q/%q; refusing to overwrite silently",
			existing.Correlation, existing.RunID, current.Correlation, current.RunID,
		)
	}
	return existing, nil
}
