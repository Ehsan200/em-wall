package rules

import (
	"fmt"
	"math/rand"
	"net"
	"testing"
)

// The Index must answer exactly what the linear matchers answer; it is a
// cache of their work, not a new policy.
func TestIndexMatchesLinear(t *testing.T) {
	labels := []string{"a", "b", "c", "google", "com", "x"}
	rng := rand.New(rand.NewSource(1))
	randName := func() string {
		n := 1 + rng.Intn(4)
		s := labels[rng.Intn(len(labels))]
		for i := 1; i < n; i++ {
			s = labels[rng.Intn(len(labels))] + "." + s
		}
		return s
	}
	for iter := 0; iter < 200; iter++ {
		var rs []Rule
		for i := 0; i < 1+rng.Intn(30); i++ {
			p := randName()
			switch rng.Intn(4) {
			case 0:
				p = "*." + p
			case 1:
				p = fmt.Sprintf("10.%d.0.0/%d", rng.Intn(3), 8+rng.Intn(17))
			}
			if rng.Intn(5) == 0 {
				p = "  " + p + ". " // normalization must agree too
			}
			rs = append(rs, Rule{ID: int64(rng.Intn(40) + 1), Pattern: p, Enabled: rng.Intn(6) != 0})
		}
		ix := NewIndex(rs)
		for q := 0; q < 50; q++ {
			name := randName()
			if rng.Intn(3) == 0 {
				name = "WWW." + name + "."
			}
			if got, want := ix.MostSpecific(name), MostSpecific(rs, name); got != want {
				t.Fatalf("MostSpecific(%q) = %v, want %v (rules %v)", name, got, want, rs)
			}
			ip := net.IPv4(10, byte(rng.Intn(3)), byte(rng.Intn(256)), 1)
			if got, want := ix.MostSpecificIP(ip), MostSpecificIP(rs, ip); got != want {
				t.Fatalf("MostSpecificIP(%s) = %v, want %v", ip, got, want)
			}
		}
	}
}

func BenchmarkMostSpecificLinear(b *testing.B) {
	rs := benchRules()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		MostSpecific(rs, "rr2---sn-q4fl6n6r.googlevideo.com")
	}
}

func BenchmarkMostSpecificIndex(b *testing.B) {
	ix := NewIndex(benchRules())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ix.MostSpecific("rr2---sn-q4fl6n6r.googlevideo.com")
	}
}

func benchRules() []Rule {
	rs := make([]Rule, 0, 561)
	for i := 0; i < 560; i++ {
		rs = append(rs, Rule{ID: int64(i + 1), Pattern: fmt.Sprintf("*.site%d.com", i), Enabled: true})
	}
	return append(rs, Rule{ID: 999, Pattern: "*.googlevideo.com", Enabled: true})
}
