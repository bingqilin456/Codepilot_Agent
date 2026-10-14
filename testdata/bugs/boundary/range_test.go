package boundary

import "testing"

func TestContains(t *testing.T) {
	cases := []struct {
		n    int
		want bool
	}{
		{0, false},
		{1, true},
		{3, true},
		{5, true},
		{6, false},
	}
	for _, tc := range cases {
		if got := Contains(tc.n, 1, 5); got != tc.want {
			t.Errorf("Contains(%d, 1, 5) = %v, want %v", tc.n, got, tc.want)
		}
	}
}
