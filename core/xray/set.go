package xray

import (
	"fmt"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/proxy"
)

// A Set is a user-named, ordered collection of outbounds that a rule can
// bind to as a single unit. Without it a rule has to spell out every
// member inline ("xray:a,b,c"), which means the same list is retyped per
// rule and silently drifts once the user's idea of "the set" changes.
//
// Rules store only the reference ("xrayset:NAME"); the members live in
// one row. Editing the set therefore reaches every rule that uses it,
// with no migration — the opposite of the curated-group behaviour
// documented in CLAUDE.md, where applied patterns are copied into rules
// and go stale.
//
// Semantics are exactly today's inline list: ORDERED FALLBACK, first
// member that dials wins. A set is pure indirection over an interface
// string the routing layer already understands (see ExpandMembers), so
// nothing downstream of decision.Engine needs to know sets exist.
type Set struct {
	ID   int64  `gorm:"primaryKey;column:id"`
	Name string `gorm:"not null;uniqueIndex;column:name"`
	// Members is the canonical comma-separated list of typed refs
	// ("xray:a,proxy:p1"), same tokenizer as a master entry's Dialer.
	Members   string    `gorm:"not null;column:members;type:text"`
	Enabled   bool      `gorm:"not null;default:true;column:enabled"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Set) TableName() string { return "xray_sets" }

// SetPrefix is the literal "xrayset:" used on the Rule.Interface field to
// mark a rule bound to a Set. Deliberately distinct from the "custom:"
// prefix used by custom DOMAIN groups — a set groups outbounds, not
// patterns.
const SetPrefix = "xrayset:"

// IsSetInterface reports whether stored designates an outbound set.
func IsSetInterface(s string) bool {
	return strings.HasPrefix(s, SetPrefix)
}

// SetInterface renders the stored Rule.Interface value for set name.
func SetInterface(name string) string {
	return SetPrefix + normalizeName(name)
}

// ParseSetName returns the set name referenced by a stored Interface
// field of the form "xrayset:NAME", or "" for anything else. Unlike
// ParseInterface a set field names exactly ONE set: the ordering that a
// comma list would express already lives inside the set's members.
func ParseSetName(s string) string {
	if !IsSetInterface(s) {
		return ""
	}
	return normalizeName(strings.TrimPrefix(s, SetPrefix))
}

// ParseSetMembers parses a Set.Members field. It shares the typed-ref
// tokenizer with ParseDialer but rejects the xraysub kind: a
// subscription's nodes have no local SOCKS inbound of their own, so
// they can't be a fallback target for a rule. (They are only reachable
// as balancer members inside a master entry's Dialer — see dialer.go.)
// Duplicate refs collapse to the first occurrence so the fallback order
// stays meaningful.
func ParseSetMembers(s string) ([]DialerRef, error) {
	refs, err := ParseDialer(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSetMember, err)
	}
	out := make([]DialerRef, 0, len(refs))
	seen := make(map[DialerRef]bool, len(refs))
	for _, r := range refs {
		if r.Kind == DialerKindXraysub {
			return nil, fmt.Errorf("%w: subscriptions cannot be set members "+
				"(reference %q from a master entry's dialer instead)", ErrInvalidSetMember, r.Name)
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, ErrEmptySet
	}
	return out, nil
}

// FormatSetMembers renders refs back to the canonical stored form.
func FormatSetMembers(refs []DialerRef) string { return FormatDialer(refs) }

// ExpandMembers renders refs as a literal Rule.Interface string the
// routing layer already understands, preserving order.
//
// An all-xray set expands to the plain "xray:a,b,c" form so every
// existing xray-aware code path (health badges, exit-IP labels, the
// "xray off" rule pill) keeps working unchanged. As soon as one plain
// proxy member is present the two kinds have to share one list, so the
// result switches to "proxy:..." with each xray member rewritten to its
// hidden internal proxy row (_xray_NAME) — the same rewrite
// dnsproxy.translateXrayInterface performs at resolve time.
//
// Returns "" for no refs; callers treat that as "unbound".
func ExpandMembers(refs []DialerRef) string {
	if len(refs) == 0 {
		return ""
	}
	allXray := true
	for _, r := range refs {
		if r.Kind != DialerKindXray {
			allXray = false
			break
		}
	}
	parts := make([]string, len(refs))
	if allXray {
		for i, r := range refs {
			parts[i] = r.Name
		}
		return InterfacePrefix + strings.Join(parts, ",")
	}
	for i, r := range refs {
		if r.Kind == DialerKindXray {
			parts[i] = InternalProxyName(r.Name)
		} else {
			parts[i] = r.Name
		}
	}
	return proxy.InterfacePrefix + strings.Join(parts, ",")
}

// CanonicalizeInterface normalizes a set reference to the exact form the
// expansion map is keyed by ("xrayset:name", lowercased). Non-set values
// pass through untouched.
//
// Rule.Interface is compared by exact string against that map, so a
// reference stored with different casing than the set row would silently
// miss and the rule would fail closed. Canonicalize at every write path
// that accepts a user-supplied interface rather than relying on the UI
// to send the right case.
func CanonicalizeInterface(s string) string {
	if !IsSetInterface(s) {
		return s
	}
	name := ParseSetName(s)
	if name == "" {
		return s
	}
	return SetInterface(name)
}

// ExpandSetInterface rewrites one stored Rule.Interface value using the
// supplied set expansion map (set name → literal interface, as built by
// Store.SetExpansions). Non-set inputs pass through untouched.
//
// A set reference that no longer resolves — deleted, disabled, or with
// unparseable members — is returned VERBATIM rather than blanked. No
// routing-layer code recognizes "xrayset:NAME", so such a rule fails
// closed on its own, and the dangling name stays visible in logs and in
// the daemon's cold-path readers instead of turning into a silent "".
func ExpandSetInterface(stored string, expansions map[string]string) string {
	name := ParseSetName(stored)
	if name == "" {
		return stored
	}
	if v, ok := expansions[name]; ok {
		return v
	}
	return stored
}
