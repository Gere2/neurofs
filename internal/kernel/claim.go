package kernel

import (
	"fmt"
	"regexp"
	"strings"
)

var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Claim is a statement NeuroFS can record. It is never the system of record:
// even a confirmed fact is a pointer-backed assertion about some other system.
type Claim struct {
	Scope     Scope           `json:"scope"`
	Domain    DomainID        `json:"domain"`
	Status    EpistemicStatus `json:"status"`
	Statement string          `json:"statement"`
	Evidence  []EvidenceRef   `json:"evidence,omitempty"`
}

// Validate enforces scope, domain, epistemic status, a non-empty statement,
// and the evidence rule: confirmed facts must point at evidence.
func (c Claim) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.Domain.Validate(); err != nil {
		return err
	}
	if err := c.Status.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Statement) == "" {
		return fmt.Errorf("kernel: claim statement is required")
	}
	for i, ev := range c.Evidence {
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("kernel: evidence[%d]: %w", i, err)
		}
	}
	if c.Status == StatusConfirmedFact && len(c.Evidence) == 0 {
		return fmt.Errorf("kernel: confirmed_fact requires at least one evidence pointer")
	}
	return nil
}

// Answer is an important response that must be checkable against evidence.
type Answer struct {
	Scope  Scope    `json:"scope"`
	Domain DomainID `json:"domain"`
	Body   string   `json:"body"`
	Claims []Claim  `json:"claims"`
}

// Validate checks the answer envelope and every claim.
func (a Answer) Validate() error {
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if err := a.Domain.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Body) == "" {
		return fmt.Errorf("kernel: answer body is required")
	}
	if len(a.Claims) == 0 {
		return fmt.Errorf("kernel: important answers must declare claims to verify")
	}
	for i, c := range a.Claims {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("kernel: claims[%d]: %w", i, err)
		}
		if c.Scope != a.Scope {
			return fmt.Errorf("kernel: claims[%d]: scope must match the answer", i)
		}
		if c.Domain != a.Domain {
			return fmt.Errorf("kernel: claims[%d]: domain must match the answer", i)
		}
	}
	return nil
}

// VerifyAgainstEvidence rejects answers that assert confirmed facts without
// evidence, or that mix a foreign scope/domain into the envelope. Observations
// and inferences may omit evidence (an inference typically has none).
func (a Answer) VerifyAgainstEvidence() error {
	if err := a.Validate(); err != nil {
		return err
	}
	for i, c := range a.Claims {
		if c.Status == StatusConfirmedFact {
			if len(c.Evidence) == 0 {
				return fmt.Errorf("kernel: claims[%d]: confirmed_fact is not verifiable without evidence", i)
			}
			for j, ev := range c.Evidence {
				if err := ev.Validate(); err != nil {
					return fmt.Errorf("kernel: claims[%d].evidence[%d]: %w", i, j, err)
				}
			}
		}
	}
	return nil
}
