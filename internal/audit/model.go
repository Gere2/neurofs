package audit

import "context"

// Model is the minimal surface audit.Run needs to generate a response. Real
// implementations (Anthropic, OpenAI) live outside this package so tests
// never import them. The interface is deliberately tiny so plugging a new
// provider is an afternoon, not a refactor.
type Model interface {
	// ID returns a short identifier recorded in AuditRecord.Model. Stable
	// across versions — a caller comparing runs across models relies on
	// this label.
	ID() string
	// Generate returns the model's textual response to prompt. It must not
	// panic; a transport error is a normal error return.
	Generate(ctx context.Context, prompt string) (string, error)
}

// StubModel is a deterministic, dependency-free Model for tests and
// pre-integration development. It lets callers script exact responses so
// audit metrics can be asserted against known-good outputs.
type StubModel struct {
	Label    string
	Response string
	// ResponseFn, if set, takes precedence over Response and receives the
	// full prompt — useful for tests that want to vary output by question.
	ResponseFn func(prompt string) string
}

// ID returns the stub's label (default "stub").
func (s StubModel) ID() string {
	if s.Label == "" {
		return "stub"
	}
	return s.Label
}

// Generate returns the configured response. The context is accepted so
// StubModel satisfies Model, but it is not observed — stub runs are
// synchronous by design.
func (s StubModel) Generate(_ context.Context, prompt string) (string, error) {
	if s.ResponseFn != nil {
		return s.ResponseFn(prompt), nil
	}
	return s.Response, nil
}
