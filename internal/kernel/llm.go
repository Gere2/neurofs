package kernel

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrMissingCredentials is returned when an enterprise completion is requested
// without credentials. Callers must not invent a substitute response.
var ErrMissingCredentials = errors.New("kernel: missing LLM credentials")

// ErrSilentMockForbidden is returned when a caller asks for a mock completion
// without an explicit synthetic grant. Missing credentials must never fall
// through to a placeholder string.
var ErrSilentMockForbidden = errors.New("kernel: silent LLM mock is forbidden")

// LLMMode selects the completion contract.
type LLMMode string

const (
	// ModeEnterprise never returns a mock. Missing credentials are an error.
	ModeEnterprise LLMMode = "enterprise"
	// ModeSynthetic is opt-in for tests and the local coding walkthrough.
	// It still requires AllowSynthetic; it is never inferred from an empty key.
	ModeSynthetic LLMMode = "synthetic"
)

// LLMRequest is one completion attempt.
type LLMRequest struct {
	Provider       string
	Model          string
	Prompt         string
	Mode           LLMMode
	AllowSynthetic bool
	APIKey         string
}

// Completion is a successful model (or explicitly synthetic) response.
type Completion struct {
	Text      string
	Synthetic bool
	InputTok  int
	OutputTok int
}

// Completer is the transport. Enterprise callers inject a real client;
// tests inject a fake. The kernel never silently substitutes a fake.
type Completer interface {
	Complete(ctx context.Context, req LLMRequest) (Completion, error)
}

// FailClosed is a Completer that refuses to invent text when credentials
// are missing. Inner is used only after the contract passes.
type FailClosed struct {
	Inner Completer
}

// Complete implements Completer.
func (f FailClosed) Complete(ctx context.Context, req LLMRequest) (Completion, error) {
	if err := req.validateContract(); err != nil {
		return Completion{}, err
	}
	if f.Inner == nil {
		return Completion{}, fmt.Errorf("kernel: LLM transport is not configured")
	}
	out, err := f.Inner.Complete(ctx, req)
	if err != nil {
		return Completion{}, err
	}
	if req.Mode == ModeEnterprise && out.Synthetic {
		return Completion{}, fmt.Errorf("kernel: enterprise completion must not be synthetic")
	}
	return out, nil
}

func (r LLMRequest) validateContract() error {
	switch r.Mode {
	case ModeEnterprise:
		if r.AllowSynthetic {
			return fmt.Errorf("kernel: enterprise mode cannot allow synthetic completions")
		}
		if strings.TrimSpace(r.APIKey) == "" {
			return fmt.Errorf("%w: provider %q model %q", ErrMissingCredentials, r.Provider, r.Model)
		}
	case ModeSynthetic:
		if !r.AllowSynthetic {
			return ErrSilentMockForbidden
		}
	case "":
		return fmt.Errorf("kernel: LLM mode is required")
	default:
		return fmt.Errorf("kernel: unknown LLM mode %q", r.Mode)
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return fmt.Errorf("kernel: prompt is required")
	}
	return nil
}
