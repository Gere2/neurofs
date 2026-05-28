package cli

import "fmt"

// validateBudget rejects non-positive token budgets at the CLI boundary.
// Without this check, --budget -1 silently produced bundles whose stats
// line showed "tokens used : 80 / -1 (-8000.0%)" — the QA agent flagged
// it as confusing and a sign of missing input validation. Anything below
// 1 cannot possibly fit any fragment, so the error message is honest.
func validateBudget(budget int) error {
	if budget < 1 {
		return fmt.Errorf("--budget must be >= 1 (got %d)", budget)
	}
	return nil
}
