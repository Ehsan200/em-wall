package rules

import (
	"net"
	"strings"
	"time"
)

type Action string

const (
	// ActionBlock returns NXDOMAIN for matching queries.
	ActionBlock Action = "block"
	// ActionAllow lets matching queries through the default route,
	// overriding any broader block rule. Interface MUST be empty.
	ActionAllow Action = "allow"
	// ActionRoute lets matching queries through and pins their
	// resolved IPs to a specific interface (utunN), proxy:NAME, or
	// xray:NAME. Interface MUST be non-empty.
	ActionRoute Action = "route"
)

// ValidAction reports whether v is one of the recognised actions.
func ValidAction(v Action) bool {
	switch v {
	case ActionBlock, ActionAllow, ActionRoute:
		return true
	}
	return false
}

type Rule struct {
	ID        int64     `gorm:"primaryKey;column:id"`
	Pattern   string    `gorm:"not null;uniqueIndex;column:pattern"`
	Action    Action    `gorm:"not null;column:action;type:text"`
	Interface string    `gorm:"not null;default:'';column:interface"`
	Enabled   bool      `gorm:"not null;default:true;column:enabled"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	return s
}

// ParseCIDR interprets pattern as a destination IP or CIDR rule. A bare
// address ("1.2.3.4", "2001:db8::1") becomes a host route (/32 or /128);
// a "addr/bits" string is parsed as a network. Returns ok=false for
// anything that isn't an IP/CIDR — i.e. a normal domain pattern.
func ParseCIDR(pattern string) (*net.IPNet, bool) {
	p := normalize(pattern)
	if p == "" {
		return nil, false
	}
	if ip := net.ParseIP(p); ip != nil {
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, true
	}
	if _, n, err := net.ParseCIDR(p); err == nil {
		return n, true
	}
	return nil, false
}

// IsIPRule reports whether pattern is a destination IP/CIDR (vs a domain).
func IsIPRule(pattern string) bool {
	_, ok := ParseCIDR(pattern)
	return ok
}

// Match reports whether name matches pattern.
// A pattern of "*.y.com" matches "y.com", "a.y.com", and "a.b.y.com".
// A pattern of "y.com" matches only "y.com".
func Match(pattern, name string) bool {
	p := normalize(pattern)
	n := normalize(name)
	if p == "" || n == "" {
		return false
	}
	if strings.HasPrefix(p, "*.") {
		suffix := p[2:]
		return n == suffix || strings.HasSuffix(n, "."+suffix)
	}
	return p == n
}

// Specificity returns a comparable score for resolving rule conflicts.
// Higher is more specific. Score = 2*literal_label_count + (exact ? 1 : 0).
// An exact rule outranks any wildcard match at the same depth; deeper
// patterns outrank shallower ones.
func Specificity(pattern string) int {
	p := normalize(pattern)
	if p == "" {
		return 0
	}
	exact := !strings.HasPrefix(p, "*.")
	body := p
	if !exact {
		body = p[2:]
	}
	literals := len(strings.Split(body, "."))
	score := literals * 2
	if exact {
		score++
	}
	return score
}

// Validate returns nil if the pattern is well-formed. A pattern is valid
// if it is either a domain pattern (optionally wildcarded) or a
// destination IP/CIDR.
func Validate(pattern string) error {
	p := normalize(pattern)
	if p == "" {
		return ErrEmptyPattern
	}
	if strings.Contains(p, " ") {
		return ErrInvalidPattern
	}
	// IP / CIDR rule: accept as-is, skip the domain-label checks below.
	if IsIPRule(p) {
		return nil
	}
	body := p
	if strings.HasPrefix(body, "*.") {
		body = body[2:]
	}
	if body == "" || strings.Contains(body, "*") {
		return ErrInvalidPattern
	}
	for _, label := range strings.Split(body, ".") {
		if label == "" {
			return ErrInvalidPattern
		}
		for _, r := range label {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !isAlnum && r != '-' {
				return ErrInvalidPattern
			}
		}
	}
	return nil
}

// MostSpecific returns the most specific matching rule for name, or nil.
// Disabled rules are skipped. On ties, exact rules beat wildcards;
// further ties are broken by lower ID (older rule wins).
func MostSpecific(rules []Rule, name string) *Rule {
	var best *Rule
	bestScore := -1
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		if IsIPRule(r.Pattern) {
			continue // IP/CIDR rules are matched by MostSpecificIP, not by name
		}
		if !Match(r.Pattern, name) {
			continue
		}
		score := Specificity(r.Pattern)
		if score > bestScore || (score == bestScore && best != nil && r.ID < best.ID) {
			best = r
			bestScore = score
		}
	}
	return best
}

// MostSpecificIP returns the most specific IP/CIDR rule whose network
// contains ip, or nil. "Most specific" means the longest prefix (largest
// mask); ties are broken by lower ID (older rule wins). Disabled rules and
// domain rules are skipped.
func MostSpecificIP(rules []Rule, ip net.IP) *Rule {
	var best *Rule
	bestOnes := -1
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		n, ok := ParseCIDR(r.Pattern)
		if !ok || !n.Contains(ip) {
			continue
		}
		ones, _ := n.Mask.Size()
		if ones > bestOnes || (ones == bestOnes && best != nil && r.ID < best.ID) {
			best = r
			bestOnes = ones
		}
	}
	return best
}
