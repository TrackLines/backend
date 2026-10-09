package boards

import "testing"

func TestPoints(t *testing.T) {
	s := func(v string) *string { return &v }
	for in, want := range map[*string]float64{nil: 0, s("8"): 8, s("M"): 3, s("XL"): 8, s("junk"): 0} {
		if got := Points(in); got != want {
			t.Errorf("Points(%v) = %v, want %v", in, got, want)
		}
	}
}
