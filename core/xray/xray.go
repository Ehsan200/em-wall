// Package xray models user-defined xray-core outbound configurations
// the daemon can route DNS-matched traffic through. A rule with
// Interface = "xray:NAME[,NAME2]" routes its matching IPs into a local
// SOCKS5 inbound that an embedded xray subprocess maps 1:1 to the
// outbound named NAME via inbound/outbound tags.
//
// Each entry stores a raw JSON outbound block (the part of xray's
// config under "outbounds[i]"). The supervisor generates one composite
// xray config from all enabled entries — pairing inbound tag
// "in-NAME" with outbound tag "out-NAME" via a routing rule — and
// also keeps a hidden proxy.Proxy row (Name = "_xray_NAME", protocol
// socks5, 127.0.0.1:SocksPort) in sync so the existing proxy plumbing
// (proxytun + netstack) handles the actual TCP/UDP forwarding.
package xray

import (
	"strings"
	"time"
)

// Config is one user-named outbound. SocksPort is the loopback port
// the supervisor binds for this entry's SOCKS5 inbound — stable for
// the lifetime of the row so existing connections don't move.
type Config struct {
	ID        int64  `gorm:"primaryKey;column:id"`
	Name      string `gorm:"not null;uniqueIndex;column:name"`
	Outbound  string `gorm:"not null;column:outbound;type:text"`
	SocksPort int    `gorm:"not null;uniqueIndex;column:socks_port"`
	Enabled   bool   `gorm:"not null;default:true;column:enabled"`
	// Dialer, when non-empty, makes this a "master" entry: its outbound's
	// streamSettings.sockopt.dialerProxy is wired through a per-master
	// leastPing balancer over the referenced nodes. Comma-separated typed
	// refs — see ParseDialer. Empty for an ordinary entry.
	Dialer    string    `gorm:"column:dialer;type:text;not null;default:''"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Config) TableName() string { return "xray_configs" }

// InterfacePrefix is the literal "xray:" used on the Rule.Interface
// field to mark an xray-routed rule.
const InterfacePrefix = "xray:"

// IsXrayInterface reports whether stored designates one or more xray
// outbounds.
func IsXrayInterface(s string) bool {
	return strings.HasPrefix(s, InterfacePrefix)
}

// ParseInterface returns the xray entry names referenced by a stored
// Interface field of the form "xray:NAME[,NAME...]". Whitespace around
// names is trimmed; empty names are dropped. Returns nil for fields
// that don't start with "xray:".
//
// Mirrors proxy.ParseInterface so multi-entry fallback semantics
// ("first available") work the same way.
func ParseInterface(s string) []string {
	if !IsXrayInterface(s) {
		return nil
	}
	raw := strings.TrimPrefix(s, InterfacePrefix)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidName reports whether name is well-formed for an xray entry.
// Same character class as proxy names so the comma-separated Interface
// field has a single tokenizer rule.
func ValidName(name string) bool {
	n := normalizeName(name)
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// InternalProxyName is the hidden proxy.Proxy name the supervisor
// mints for each Config — Add/Sync writes a corresponding row into
// the proxy store. dnsproxy rewrites "xray:NAME" to this internal
// proxy name at resolve time so the existing forwarder handles dial.
func InternalProxyName(name string) string {
	return InternalProxyPrefix + normalizeName(name)
}

// InternalProxyPrefix marks proxy.Proxy rows minted by the xray
// supervisor. The user-facing Proxies UI MUST filter rows whose name
// starts with this prefix so the auto-managed entries don't show up
// alongside the user's own.
const InternalProxyPrefix = "_xray_"

// IsInternalProxyName reports whether a proxy.Proxy.Name was minted
// by the xray supervisor.
func IsInternalProxyName(name string) bool {
	return strings.HasPrefix(name, InternalProxyPrefix)
}

// InboundTag is the xray inbound tag for entry NAME. Used both in the
// generated config's inbound entry and in the routing rule that pairs
// it with the matching outbound.
func InboundTag(name string) string { return "in-" + normalizeName(name) }

// OutboundTag is the xray outbound tag for entry NAME. Overwrites any
// "tag" the user set inside their outbound JSON so the routing rule
// is guaranteed to match.
func OutboundTag(name string) string { return "out-" + normalizeName(name) }

// Tags always present in the generated config so user routing rules
// can target them by name. TagDirect emits the original destination
// (xray's "freedom" protocol); TagBlock blackholes the connection.
const (
	TagDirect = "direct"
	TagBlock  = "block"
)

// SettingRoutingKey is the key under which the user-supplied global
// routing-rule JSON array is stored in xray_settings.
const SettingRoutingKey = "routing_rules"
