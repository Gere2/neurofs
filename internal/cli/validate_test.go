package cli

import "testing"

func TestValidateBudget(t *testing.T) {
	cases := []struct {
		in      int
		wantErr bool
	}{
		{-1, true},
		{0, true},
		{1, false},
		{8000, false},
		{1 << 30, false},
	}
	for _, c := range cases {
		err := validateBudget(c.in)
		got := err != nil
		if got != c.wantErr {
			t.Errorf("validateBudget(%d) error=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}
