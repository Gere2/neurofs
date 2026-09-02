package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testScope() Scope {
	return LocalCodeScope("github.com/Gere2/neurofs")
}

func TestScopeRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := (Scope{}).Validate(); err == nil {
		t.Fatal("empty scope must be invalid")
	}
	if err := (Scope{OrganizationID: "acme"}).Validate(); err == nil {
		t.Fatal("missing tenant must be invalid")
	}
}

func TestLocalCodeScopeIsExplicit(t *testing.T) {
	t.Parallel()
	s := LocalCodeScope("github.com/Gere2/neurofs")
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.OrganizationID != "local" {
		t.Fatalf("organization = %q, want local", s.OrganizationID)
	}
}

func TestEpistemicStatusClosedSet(t *testing.T) {
	t.Parallel()
	for _, s := range []EpistemicStatus{StatusObservation, StatusInference, StatusConfirmedFact} {
		if err := s.Validate(); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := EpistemicStatus("probably").Validate(); err == nil {
		t.Fatal("unknown status must be invalid")
	}
}

func TestConfirmedFactRequiresEvidence(t *testing.T) {
	t.Parallel()
	c := Claim{
		Scope:     testScope(),
		Domain:    DomainCode,
		Status:    StatusConfirmedFact,
		Statement: "weightFilename is the filename match weight",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("confirmed_fact without evidence must be invalid")
	}
	c.Evidence = []EvidenceRef{{
		System:        "audit_bundle",
		Locator:       "audit/bundles/example.json",
		ContentSHA256: strings.Repeat("ab", 32),
	}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestObservationMayOmitEvidence(t *testing.T) {
	t.Parallel()
	c := Claim{
		Scope:     testScope(),
		Domain:    DomainCode,
		Status:    StatusObservation,
		Statement: "bundle listed internal/ranking/weights.go",
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerVerifyAgainstEvidence(t *testing.T) {
	t.Parallel()
	scope := testScope()
	ev := EvidenceRef{System: "git+fs", Locator: "internal/ranking/weights.go#L40"}
	ans := Answer{
		Scope:  scope,
		Domain: DomainCode,
		Body:   "The ranker uses weightFilename.",
		Claims: []Claim{
			{
				Scope:     scope,
				Domain:    DomainCode,
				Status:    StatusObservation,
				Statement: "weights.go is in the bundle",
				Evidence:  []EvidenceRef{ev},
			},
			{
				Scope:     scope,
				Domain:    DomainCode,
				Status:    StatusInference,
				Statement: "filename matches probably dominate this query",
			},
			{
				Scope:     scope,
				Domain:    DomainCode,
				Status:    StatusConfirmedFact,
				Statement: "weightFilename is defined in weights.go",
				Evidence:  []EvidenceRef{ev},
			},
		},
	}
	if err := ans.VerifyAgainstEvidence(); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerRejectsForeignScope(t *testing.T) {
	t.Parallel()
	scope := testScope()
	ans := Answer{
		Scope:  scope,
		Domain: DomainCode,
		Body:   "x",
		Claims: []Claim{{
			Scope:     Scope{OrganizationID: "other", TenantID: "t"},
			Domain:    DomainCode,
			Status:    StatusObservation,
			Statement: "x",
		}},
	}
	if err := ans.Validate(); err == nil {
		t.Fatal("claim scope mismatch must be invalid")
	}
}

func TestPACRRejectsSkippedApproval(t *testing.T) {
	t.Parallel()
	scope := testScope()
	a := Action{
		Scope:    scope,
		Domain:   DomainCode,
		Status:   StatusCommanded,
		Proposal: Proposal{ID: "p1", Scope: scope, Domain: DomainCode, Intent: "touch Raíz"},
		Command:  &Command{ID: "c1", ApprovalID: "missing"},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("command without approval must be invalid")
	}
}

func TestPACRHappyPath(t *testing.T) {
	t.Parallel()
	scope := testScope()
	p := Proposal{ID: "p1", Scope: scope, Domain: DomainCode, Intent: "run go test"}
	ap := &Approval{ID: "a1", ProposalID: "p1", Actor: "owner", Decision: "approve"}
	cmd := &Command{ID: "c1", ApprovalID: "a1"}
	rec := &ActionReceipt{
		ID:        "r1",
		CommandID: "c1",
		Evidence:  EvidenceRef{System: "run_receipt", Locator: "audit/run_receipts.jsonl"},
	}
	steps := []Action{
		{Scope: scope, Domain: DomainCode, Status: StatusProposed, Proposal: p},
		{Scope: scope, Domain: DomainCode, Status: StatusApproved, Proposal: p, Approval: ap},
		{Scope: scope, Domain: DomainCode, Status: StatusCommanded, Proposal: p, Approval: ap, Command: cmd},
		{Scope: scope, Domain: DomainCode, Status: StatusReceipted, Proposal: p, Approval: ap, Command: cmd, Receipt: rec},
	}
	for _, step := range steps {
		if err := step.Validate(); err != nil {
			t.Fatalf("status %s: %v", step.Status, err)
		}
	}
}

type stubCompleter struct {
	out Completion
	err error
}

func (s stubCompleter) Complete(context.Context, LLMRequest) (Completion, error) {
	return s.out, s.err
}

func TestEnterpriseLLMMissingKeyFailsClosed(t *testing.T) {
	t.Parallel()
	client := FailClosed{Inner: stubCompleter{out: Completion{Text: "should not appear"}}}
	out, err := client.Complete(context.Background(), LLMRequest{
		Provider: "anthropic",
		Model:    "claude",
		Prompt:   "summarise",
		Mode:     ModeEnterprise,
	})
	if !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("err = %v, want ErrMissingCredentials", err)
	}
	if out.Text != "" {
		t.Fatalf("fail-closed must not return text, got %q", out.Text)
	}
}

func TestSilentMockForbiddenWithoutGrant(t *testing.T) {
	t.Parallel()
	client := FailClosed{Inner: stubCompleter{out: Completion{Text: "[Mock]", Synthetic: true}}}
	_, err := client.Complete(context.Background(), LLMRequest{
		Prompt: "x",
		Mode:   ModeSynthetic,
	})
	if !errors.Is(err, ErrSilentMockForbidden) {
		t.Fatalf("err = %v, want ErrSilentMockForbidden", err)
	}
}

func TestSyntheticAllowedWhenExplicit(t *testing.T) {
	t.Parallel()
	client := FailClosed{Inner: stubCompleter{out: Completion{Text: "[Mock]", Synthetic: true}}}
	out, err := client.Complete(context.Background(), LLMRequest{
		Prompt:         "x",
		Mode:           ModeSynthetic,
		AllowSynthetic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Synthetic || out.Text != "[Mock]" {
		t.Fatalf("got %+v", out)
	}
}

func TestEnterpriseRejectsSyntheticInner(t *testing.T) {
	t.Parallel()
	client := FailClosed{Inner: stubCompleter{out: Completion{Text: "[Mock]", Synthetic: true}}}
	_, err := client.Complete(context.Background(), LLMRequest{
		Prompt: "x",
		Mode:   ModeEnterprise,
		APIKey: "sk-test",
	})
	if err == nil {
		t.Fatal("enterprise must reject synthetic inner completions")
	}
}
