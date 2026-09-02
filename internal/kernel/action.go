package kernel

import (
	"fmt"
	"strings"
)

// ActionStatus is the only allowed path for mutating work against an external
// system. NeuroFS does not skip from proposal to receipt.
type ActionStatus string

const (
	StatusProposed  ActionStatus = "proposed"
	StatusApproved  ActionStatus = "approved"
	StatusCommanded ActionStatus = "commanded"
	StatusReceipted ActionStatus = "receipted"
	StatusRejected  ActionStatus = "rejected"
)

// Proposal is an intent to change something in a system of record NeuroFS
// does not own. It is not an execution.
type Proposal struct {
	ID     string   `json:"id"`
	Scope  Scope    `json:"scope"`
	Domain DomainID `json:"domain"`
	Intent string   `json:"intent"`
}

// Approval is a recorded decision on a proposal. Without it, no command.
type Approval struct {
	ID         string `json:"id"`
	ProposalID string `json:"proposal_id"`
	Actor      string `json:"actor"`
	Decision   string `json:"decision"` // "approve" or "reject"
}

// Command is the dispatch of an approved proposal to an external executor.
type Command struct {
	ID         string `json:"id"`
	ApprovalID string `json:"approval_id"`
}

// ActionReceipt is evidence that a command was attempted. It is not the SoR
// record the command may have mutated.
type ActionReceipt struct {
	ID        string      `json:"id"`
	CommandID string      `json:"command_id"`
	Evidence  EvidenceRef `json:"evidence"`
}

// Action is one PACR chain. Status is derived from which steps are present
// only after Validate succeeds; callers set Status explicitly and Validate
// checks it matches the chain.
type Action struct {
	Scope    Scope          `json:"scope"`
	Domain   DomainID       `json:"domain"`
	Status   ActionStatus   `json:"status"`
	Proposal Proposal       `json:"proposal"`
	Approval *Approval      `json:"approval,omitempty"`
	Command  *Command       `json:"command,omitempty"`
	Receipt  *ActionReceipt `json:"receipt,omitempty"`
}

// Validate enforces Proposal → Approval → Command → Receipt (or reject after
// proposal). No step may appear without its predecessor.
func (a Action) Validate() error {
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if err := a.Domain.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Proposal.ID) == "" || strings.TrimSpace(a.Proposal.Intent) == "" {
		return fmt.Errorf("kernel: proposal id and intent are required")
	}
	if a.Proposal.Scope != a.Scope || a.Proposal.Domain != a.Domain {
		return fmt.Errorf("kernel: proposal scope and domain must match the action")
	}

	switch a.Status {
	case StatusProposed:
		if a.Approval != nil || a.Command != nil || a.Receipt != nil {
			return fmt.Errorf("kernel: proposed action cannot carry later PACR steps")
		}
	case StatusRejected:
		if a.Approval == nil {
			return fmt.Errorf("kernel: rejected action requires an approval record")
		}
		if a.Approval.Decision != "reject" {
			return fmt.Errorf("kernel: rejected action requires approval.decision=reject")
		}
		if a.Command != nil || a.Receipt != nil {
			return fmt.Errorf("kernel: rejected action cannot carry command or receipt")
		}
		if a.Approval.ProposalID != a.Proposal.ID {
			return fmt.Errorf("kernel: approval.proposal_id must match proposal.id")
		}
	case StatusApproved:
		if err := a.requireApproval("approve"); err != nil {
			return err
		}
		if a.Command != nil || a.Receipt != nil {
			return fmt.Errorf("kernel: approved action cannot carry command or receipt yet")
		}
	case StatusCommanded:
		if err := a.requireApproval("approve"); err != nil {
			return err
		}
		if a.Command == nil || strings.TrimSpace(a.Command.ID) == "" {
			return fmt.Errorf("kernel: commanded action requires a command")
		}
		if a.Command.ApprovalID != a.Approval.ID {
			return fmt.Errorf("kernel: command.approval_id must match approval.id")
		}
		if a.Receipt != nil {
			return fmt.Errorf("kernel: commanded action cannot carry a receipt yet")
		}
	case StatusReceipted:
		if err := a.requireApproval("approve"); err != nil {
			return err
		}
		if a.Command == nil {
			return fmt.Errorf("kernel: receipted action requires a command")
		}
		if a.Receipt == nil {
			return fmt.Errorf("kernel: receipted action requires a receipt")
		}
		if a.Receipt.CommandID != a.Command.ID {
			return fmt.Errorf("kernel: receipt.command_id must match command.id")
		}
		if err := a.Receipt.Evidence.Validate(); err != nil {
			return err
		}
	default:
		if a.Status == "" {
			return fmt.Errorf("kernel: action status is required")
		}
		return fmt.Errorf("kernel: unknown action status %q", a.Status)
	}
	return nil
}

func (a Action) requireApproval(want string) error {
	if a.Approval == nil {
		return fmt.Errorf("kernel: action status %q requires an approval", a.Status)
	}
	if strings.TrimSpace(a.Approval.ID) == "" || strings.TrimSpace(a.Approval.Actor) == "" {
		return fmt.Errorf("kernel: approval id and actor are required")
	}
	if a.Approval.ProposalID != a.Proposal.ID {
		return fmt.Errorf("kernel: approval.proposal_id must match proposal.id")
	}
	if a.Approval.Decision != want {
		return fmt.Errorf("kernel: approval.decision must be %q for status %q", want, a.Status)
	}
	return nil
}
