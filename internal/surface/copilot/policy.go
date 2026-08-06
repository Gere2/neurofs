// Package copilot observes GitHub Copilot CLI runs.
//
// It is an observer, not a router. It never picks a model to optimize cost or
// quality — that is the provider's router's job, and duplicating it is the one
// thing this layer deliberately does not do. What it does is apply the owner's
// policy (which models are permissible at all), launch the CLI, and preserve
// what happened as evidence: argv, exit status, duration, whatever model
// identities the run disclosed, and the raw output as hashed artifacts.
//
// It issues no extra model calls. Every figure it cannot prove is recorded as
// unknown rather than estimated.
package copilot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gere2/neurofs/internal/receipt"
)

// Surface is the receipt surface name for this adapter.
const Surface = "copilot_cli"

// AllowedModelsRelPath is the repository-level model allow-list that Copilot
// CLI honors. Its presence is the only model restriction this adapter can
// demonstrate is enforced by the provider.
//
// It is not a complete guarantee: the allow-list does not cover BYOK models.
// Enforcement is therefore reported as provider_enforced only for the catalog
// it does cover, and an owner who needs an absolute restriction must pin a
// model instead of trusting it (see PlanSelection).
const AllowedModelsRelPath = ".github/allowed_models.txt"

// SelectionPolicy is the owner's stance on model choice.
type SelectionPolicy struct {
	// RequireEnforcedRestriction means the owner needs a demonstrable hard
	// restriction — for example because the repository must never reach a
	// model that retains prompts. When enforcement cannot be demonstrated,
	// auto routing is refused rather than trusted.
	RequireEnforcedRestriction bool
	// PinnedModel, when set, is the model the policy selects explicitly. It is
	// the fail-closed fallback, not an optimization: pinning is how the owner
	// restricts the catalog, not how NeuroFS competes with the router.
	PinnedModel string
}

// DetectEnforcement reports how a model restriction is enforced for a repo,
// along with the allow-listed models it found.
//
// A missing or empty allow-list is reported as unknown, never as "no
// restriction needed" — the caller decides what to do with that, and
// PlanSelection makes the fail-closed choice explicit.
func DetectEnforcement(repoRoot string) (receipt.Enforcement, []string, error) {
	path := filepath.Join(repoRoot, filepath.FromSlash(AllowedModelsRelPath))
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return receipt.EnforcementUnknown, nil, nil
		}
		return receipt.EnforcementUnknown, nil, fmt.Errorf("copilot: read allow-list: %w", err)
	}
	defer func() { _ = f.Close() }()

	var models []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		models = append(models, line)
	}
	if err := scanner.Err(); err != nil {
		return receipt.EnforcementUnknown, nil, fmt.Errorf("copilot: scan allow-list: %w", err)
	}
	if len(models) == 0 {
		return receipt.EnforcementUnknown, nil, nil
	}
	return receipt.EnforcementProvider, models, nil
}

// PlanSelection is the fail-closed rule.
//
// When the owner requires a demonstrable restriction and none can be shown,
// auto routing is refused: the run either pins an allowed model or does not
// happen. Letting the router choose freely "because probably nothing bad will
// happen" is precisely the silent failure this layer exists to prevent.
func PlanSelection(p SelectionPolicy, enforcement receipt.Enforcement, allowed []string) (receipt.SelectionMode, string, error) {
	if p.PinnedModel != "" {
		if len(allowed) > 0 && !contains(allowed, p.PinnedModel) {
			return "", "", fmt.Errorf(
				"copilot: pinned model %q is not in the repository allow-list %v",
				p.PinnedModel, allowed)
		}
		return receipt.SelectionExplicit, p.PinnedModel, nil
	}
	if p.RequireEnforcedRestriction && enforcement != receipt.EnforcementProvider {
		return "", "", fmt.Errorf(
			"copilot: policy requires an enforced model restriction but enforcement is %q; "+
				"add %s or pin a model — refusing to let the router choose freely",
			enforcement, AllowedModelsRelPath)
	}
	return receipt.SelectionAuto, "", nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
