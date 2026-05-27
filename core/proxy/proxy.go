// Package proxy holds the model + storage for upstream proxies the
// daemon can route traffic through. A rule with Interface = "proxy:NAME"
// (or "proxy:NAME1,NAME2") routes its matching DNS answers' IPs to a
// daemon-owned utun where a user-space TCP stack terminates the
// connection and forwards it through the named proxy. Phase A wires up
// the model, IPC, and UI only; interception lands in Phase B.
package proxy

import (
	"strings"
	"time"
)

type Protocol string

const (
	// ProtocolSOCKS5 — SOCKS5 with optional username/password
	// authentication (RFC 1928 / RFC 1929). Supports CONNECT for TCP
	// and UDP ASSOCIATE for UDP (Phase C).
	ProtocolSOCKS5 Protocol = "socks5"
	// ProtocolHTTP — HTTP CONNECT tunneling (RFC 7231 §4.3.6) with
	// optional Basic auth. TCP only.
	ProtocolHTTP Protocol = "http"
)

// ValidProtocol reports whether v is a supported proxy protocol.
func ValidProtocol(v Protocol) bool {
	switch v {
	case ProtocolSOCKS5, ProtocolHTTP:
		return true
	}
	return false
}

// Proxy is an upstream proxy server the daemon can forward traffic
// through. The Name field is the identifier rules reference via
// Interface = "proxy:NAME".
type Proxy struct {
	ID        int64     `gorm:"primaryKey;column:id"`
	Name      string    `gorm:"not null;uniqueIndex;column:name"`
	Protocol  Protocol  `gorm:"not null;column:protocol;type:text"`
	Host      string    `gorm:"not null;column:host"`
	Port      int       `gorm:"not null;column:port"`
	Username  string    `gorm:"not null;default:'';column:username"`
	Password  string    `gorm:"not null;default:'';column:password"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Proxy) TableName() string { return "proxies" }

// InterfacePrefix is the literal "proxy:" used on the Rule.Interface
// field to mark a proxy-routed rule.
const InterfacePrefix = "proxy:"

// IsProxyInterface reports whether the stored Rule.Interface field
// designates one or more proxies.
func IsProxyInterface(s string) bool {
	return strings.HasPrefix(s, InterfacePrefix)
}

// ParseInterface returns the proxy names referenced by a stored
// Interface field of the form "proxy:NAME[,NAME...]". Whitespace around
// names is trimmed; empty names are dropped. Returns nil for fields
// that don't start with "proxy:".
//
// Mirrors core/dnsproxy.parseAppKeys for the app:KEY case so multi-
// proxy fallback semantics ("first available") work the same way.
func ParseInterface(s string) []string {
	if !IsProxyInterface(s) {
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

// ValidName reports whether name is well-formed for a proxy. Allowed
// characters: lowercase alphanumeric, dash, underscore. Length 1..64.
// Matches what looks unambiguous in a comma-separated Interface field.
func ValidName(name string) bool {
	n := normalizeName(name)
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
