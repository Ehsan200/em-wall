package xray

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A master entry's Dialer field is a comma-separated list of typed refs
// naming the outbounds its sockopt.dialerProxy chain draws from. All
// referenced nodes are merged into one per-master leastPing balancer, so
// the master's own server connection is tunneled through whichever of
// them the observatory currently rates fastest.
const (
	DialerKindXray    = "xray"    // a single user xray entry
	DialerKindXraysub = "xraysub" // all active nodes of a subscription
	DialerKindProxy   = "proxy"   // a user proxy upstream
)

// Observatory probe defaults (user-overridable via GenerateOptions).
const (
	DefaultProbeURL      = "http://www.gstatic.com/generate_204"
	DefaultProbeInterval = "60s"
	// ObservatorySelectorPrefix matches every slot member outbound so a
	// single top-level observatory feeds all per-master balancers.
	ObservatorySelectorPrefix = "slot"
)

// DialerRef is one parsed entry of a master's Dialer field.
type DialerRef struct {
	Kind string // DialerKind*
	Name string // normalized entry/subscription/proxy name
}

// ParseDialer parses a Dialer field ("xray:a,xraysub:b,proxy:c"). Empty
// input yields (nil, nil). Every item must carry a known kind prefix and
// a valid name.
func ParseDialer(s string) ([]DialerRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []DialerRef
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kind, name, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("%w: %q missing kind prefix (want xray:/xraysub:/proxy:)", ErrInvalidDialer, p)
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		name = normalizeName(name)
		switch kind {
		case DialerKindXray, DialerKindXraysub, DialerKindProxy:
		default:
			return nil, fmt.Errorf("%w: unknown kind %q in %q", ErrInvalidDialer, kind, p)
		}
		if !ValidName(name) {
			return nil, fmt.Errorf("%w: invalid name in %q", ErrInvalidDialer, p)
		}
		out = append(out, DialerRef{Kind: kind, Name: name})
	}
	return out, nil
}

// FormatDialer renders refs back to the canonical stored form.
func FormatDialer(refs []DialerRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.Kind + ":" + r.Name
	}
	return strings.Join(parts, ",")
}

// RefsByKind groups ref names by kind for daemon-side existence checks.
func RefsByKind(refs []DialerRef) (xrayNames, subNames, proxyNames []string) {
	for _, r := range refs {
		switch r.Kind {
		case DialerKindXray:
			xrayNames = append(xrayNames, r.Name)
		case DialerKindXraysub:
			subNames = append(subNames, r.Name)
		case DialerKindProxy:
			proxyNames = append(proxyNames, r.Name)
		}
	}
	return
}

// DialerMember is one resolved outbound loaded into a master's slot. Key
// is a stable identity (node fingerprint, or "xray-NAME"/"proxy-NAME")
// used to build the member's tag so live add/remove (P4) can target it.
type DialerMember struct {
	Key      string
	Outbound json.RawMessage
}

// DialerSlot is a fully resolved per-master balancer slot: the master
// entry whose sockopt is wired to it, the slot index (→ port + tags),
// and the member outbounds the balancer selects among.
type DialerSlot struct {
	Master  string
	Index   int
	Members []DialerMember
}

// Slot tag / port helpers. A slot's member outbounds all share the
// SlotOutboundPrefix so the balancer selector and the shared observatory
// pick them up by prefix — the key to live add/remove without a reload.
func SlotInboundTag(i int) string     { return fmt.Sprintf("slot%d-in", i) }
func SlotBalancerTag(i int) string    { return fmt.Sprintf("slot%d-bal", i) }
func SlotOutboundPrefix(i int) string { return fmt.Sprintf("slot%d-out-", i) }
func SlotMemberTag(i int, key string) string {
	return SlotOutboundPrefix(i) + sanitizeTag(key)
}

// DialerOutboundTag is the stable socks outbound a master's dialerProxy
// points at; it forwards to the slot's inbound → balancer.
func DialerOutboundTag(master string) string { return "dialer-" + normalizeName(master) }

// SlotPort maps a slot index to its loopback inbound port.
func SlotPort(i int) int { return SlotPortStart + i }

// DetectDialerCycle reports whether treating (selfID, selfName, selfDialer)
// as an entry — replacing the stored row with id selfID (use 0 on add) —
// creates a cycle in the master-dialer graph. Only xray: refs can loop
// (xraysub:/proxy: are leaves). Used to reject a dialer that would route an
// entry's transport through itself, directly or transitively.
func DetectDialerCycle(entries []Config, selfID int64, selfName, selfDialer string) bool {
	adj := make(map[string][]string, len(entries)+1)
	for _, c := range entries {
		if c.ID == selfID {
			continue // replaced by the proposed refs below
		}
		refs, err := ParseDialer(c.Dialer)
		if err != nil {
			continue
		}
		x, _, _ := RefsByKind(refs)
		adj[normalizeName(c.Name)] = x
	}
	self := normalizeName(selfName)
	srefs, _ := ParseDialer(selfDialer)
	sx, _, _ := RefsByKind(srefs)
	adj[self] = sx

	const (
		visiting = 1
		done     = 2
	)
	color := make(map[string]int, len(adj))
	var dfs func(n string) bool
	dfs = func(n string) bool {
		color[n] = visiting
		for _, m := range adj[n] {
			if color[m] == visiting {
				return true
			}
			if color[m] == 0 && dfs(m) {
				return true
			}
		}
		color[n] = done
		return false
	}
	return dfs(self)
}

// MemberOutboundJSON returns a slot member's outbound with its tag set
// (and legacy xhttp shapes healed) — identical to what Generate bakes, so
// a live `xray api ado` adds a byte-for-byte equivalent outbound. Wrap the
// result in {"outbounds":[...]} to feed the CLI.
func MemberOutboundJSON(slotIndex int, key string, raw []byte) (json.RawMessage, error) {
	var ob map[string]any
	if err := json.Unmarshal(raw, &ob); err != nil {
		return nil, fmt.Errorf("xray: slot %d member %q: outbound JSON: %w", slotIndex, key, err)
	}
	ob["tag"] = SlotMemberTag(slotIndex, key)
	healXHTTPExtra(ob)
	b, err := json.Marshal(ob)
	if err != nil {
		return nil, fmt.Errorf("xray: slot %d member %q: re-marshal: %w", slotIndex, key, err)
	}
	return b, nil
}

// sanitizeTag keeps a member key safe as an xray tag suffix.
func sanitizeTag(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
