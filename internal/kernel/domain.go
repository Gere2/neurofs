package kernel

import "fmt"

// DomainID names an adapter. The plane is domain-agnostic; adapters are not.
type DomainID string

const (
	// DomainCode is the existing NeuroFS product: repositories, bundles, G1–G5.
	DomainCode DomainID = "code"
	// DomainBusiness is reserved for a future adapter. It has no implementation
	// in this repository and must not be treated as wired to Raíz or any SoR.
	DomainBusiness DomainID = "business"
)

// Validate reports whether id is a known domain.
func (d DomainID) Validate() error {
	switch d {
	case DomainCode, DomainBusiness:
		return nil
	case "":
		return fmt.Errorf("kernel: domain is required")
	default:
		return fmt.Errorf("kernel: unknown domain %q", d)
	}
}
