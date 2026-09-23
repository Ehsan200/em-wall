package rules

import (
	"net"
	"strings"
)

// Index is a rule list compiled for lookup. MostSpecific and
// MostSpecificIP re-derive everything per rule per call — normalize the
// pattern, try to parse it as an IP/CIDR, split it into labels — which on
// the DNS hot path means hundreds of allocations per query for a few
// hundred rules. An Index does that work once, when the rule list changes,
// and answers with the same semantics:
//
//   - domain: exact beats wildcard at the same depth, deeper beats
//     shallower, ties go to the lower ID;
//   - IP/CIDR: longest prefix wins, ties go to the lower ID;
//   - disabled rules never match.
//
// Returned pointers point into the slice the Index was built from.
type Index struct {
	exact map[string]*Rule // "a.b.com" → best exact rule
	wild  map[string]*Rule // "b.com" (from "*.b.com") → best wildcard rule
	ips   []ipRule
}

type ipRule struct {
	net  *net.IPNet
	ones int
	rule *Rule
}

// NewIndex compiles rs. rs must not be mutated while the Index is in use.
func NewIndex(rs []Rule) *Index {
	ix := &Index{
		exact: make(map[string]*Rule),
		wild:  make(map[string]*Rule),
	}
	for i := range rs {
		r := &rs[i]
		if !r.Enabled {
			continue
		}
		if n, ok := ParseCIDR(r.Pattern); ok {
			ones, _ := n.Mask.Size()
			ix.ips = append(ix.ips, ipRule{net: n, ones: ones, rule: r})
			continue
		}
		p := normalize(r.Pattern)
		if p == "" {
			continue
		}
		m, key := ix.exact, p
		if strings.HasPrefix(p, "*.") {
			m, key = ix.wild, p[2:]
			if key == "" {
				continue
			}
		}
		if cur, ok := m[key]; !ok || r.ID < cur.ID {
			m[key] = r
		}
	}
	return ix
}

// MostSpecific is the indexed equivalent of the package-level
// MostSpecific. Candidates are visited from most to least specific — the
// exact name, a wildcard over the name itself, then wildcards over each
// parent — so the first hit is the answer.
func (ix *Index) MostSpecific(name string) *Rule {
	n := normalize(name)
	if n == "" {
		return nil
	}
	if r, ok := ix.exact[n]; ok {
		return r
	}
	for s := n; ; {
		if r, ok := ix.wild[s]; ok {
			return r
		}
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			return nil
		}
		s = s[dot+1:]
	}
}

// MostSpecificIP is the indexed equivalent of the package-level
// MostSpecificIP.
func (ix *Index) MostSpecificIP(ip net.IP) *Rule {
	var best *Rule
	bestOnes := -1
	for _, c := range ix.ips {
		if !c.net.Contains(ip) {
			continue
		}
		if c.ones > bestOnes || (c.ones == bestOnes && c.rule.ID < best.ID) {
			best = c.rule
			bestOnes = c.ones
		}
	}
	return best
}
