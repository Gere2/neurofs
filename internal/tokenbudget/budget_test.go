package tokenbudget_test

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		text    string
		wantMin int
		wantMax int
	}{
		{"", 0, 0},
		{"hello", 1, 3},
		{"function authenticate(token string) bool", 8, 14},
	}

	for _, tc := range cases {
		got := tokenbudget.EstimateTokens(tc.text)
		if got < tc.wantMin || got > tc.wantMax {
			t.Errorf("EstimateTokens(%q) = %d, want [%d, %d]",
				tc.text, got, tc.wantMin, tc.wantMax)
		}
	}
}

func TestManagerBudget(t *testing.T) {
	m := tokenbudget.NewManager(1000)

	if m.Total() != 1000 {
		t.Errorf("Total() = %d, want 1000", m.Total())
	}
	if m.Used() != 0 {
		t.Errorf("Used() = %d, want 0", m.Used())
	}
	if m.Remaining() != 1000 {
		t.Errorf("Remaining() = %d, want 1000", m.Remaining())
	}

	m.Consume(400)

	if m.Used() != 400 {
		t.Errorf("Used() = %d, want 400", m.Used())
	}
	if m.Remaining() != 600 {
		t.Errorf("Remaining() = %d, want 600", m.Remaining())
	}
	if !m.CanFit(600) {
		t.Error("CanFit(600) should be true")
	}
	if m.CanFit(601) {
		t.Error("CanFit(601) should be false")
	}
}

func TestUtilisationPct(t *testing.T) {
	m := tokenbudget.NewManager(200)
	m.Consume(50)
	got := m.UtilisationPct()
	if got != 25.0 {
		t.Errorf("UtilisationPct() = %.2f, want 25.00", got)
	}
}
