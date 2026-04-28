package rules

import (
	"strings"
	"time"
)

type Action string

const (
	ActionBlock Action = "block"
	ActionAllow Action = "allow"
)

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

// Validate returns nil if the pattern is well-formed.
func Validate(pattern string) error {
	p := normalize(pattern)
	if p == "" {
		return ErrEmptyPattern
	}
	if strings.Contains(p, " ") {
		return ErrInvalidPattern
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
