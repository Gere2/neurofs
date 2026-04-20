// Package tokenbudget provides token estimation and budget management.
//
// Token estimation uses the widely-accepted heuristic of 1 token ≈ 4 characters.
// This is accurate enough for budget enforcement without requiring a tokenizer.
package tokenbudget

// EstimateTokens returns a rough token count for the given text.
// Uses the 4-chars-per-token approximation common across GPT-class models
// and a reasonable proxy for Claude's tokeniser as well.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return (len(text) + 3) / 4 // round up
}

// Manager tracks how many tokens remain in a budget.
type Manager struct {
	total int
	used  int
}

// NewManager creates a Manager with the given token budget.
func NewManager(budget int) *Manager {
	return &Manager{total: budget}
}

// Remaining returns the number of tokens still available.
func (m *Manager) Remaining() int {
	return m.total - m.used
}

// Total returns the original budget.
func (m *Manager) Total() int {
	return m.total
}

// Used returns how many tokens have been consumed.
func (m *Manager) Used() int {
	return m.used
}

// CanFit reports whether n tokens fit within the remaining budget.
func (m *Manager) CanFit(n int) bool {
	return n <= m.Remaining()
}

// Consume deducts n tokens from the budget.
// It does not guard against over-budget; callers should check CanFit first.
func (m *Manager) Consume(n int) {
	m.used += n
}

// UtilisationPct returns budget utilisation as a percentage (0–100).
func (m *Manager) UtilisationPct() float64 {
	if m.total == 0 {
		return 0
	}
	return float64(m.used) / float64(m.total) * 100
}
