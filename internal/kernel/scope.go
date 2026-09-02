package kernel

import (
	"fmt"
	"strings"
)

// Scope binds a datum to an organization and tenant. Local coding checkouts
// must still set both fields (typically organization "local" and a stable
// repo identity as tenant). An empty scope is invalid: there is no implicit
// global namespace.
type Scope struct {
	OrganizationID string `json:"organization_id"`
	TenantID       string `json:"tenant_id"`
}

// LocalCodeScope is the conventional scope for a single-machine coding
// checkout that has not joined a hosted tenant. TenantID should be a stable
// repo identity (module path or origin URL), not an absolute filesystem path.
func LocalCodeScope(repoIdentity string) Scope {
	return Scope{OrganizationID: "local", TenantID: strings.TrimSpace(repoIdentity)}
}

// Validate reports whether both identifiers are present and single-line.
func (s Scope) Validate() error {
	if err := validScopeID("organization_id", s.OrganizationID); err != nil {
		return err
	}
	return validScopeID("tenant_id", s.TenantID)
}

func validScopeID(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("kernel: %s is required", field)
	}
	if v != strings.TrimSpace(v) || strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("kernel: %s must be a single line", field)
	}
	return nil
}
