package update

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v2.13.0", "2.12.2", true},
		{"2.12.2", "v2.12.2", false},
		{"v2.12.1", "2.12.2", false},
		{"v3", "2.99.99", true},
		{"garbage", "2.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
