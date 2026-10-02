package xray

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// TLS / TCP fragmentation (xray freedom "fragment") for an entry's dial.
//
// DPI that blocks a server by the SNI in its TLS ClientHello reads that
// hello from the first TCP segment. Splitting the hello into small
// segments sent a few milliseconds apart defeats the cheap middleboxes
// that don't reassemble — the server reassembles normally and needs no
// change. xray does it in a freedom outbound, so an entry opts in by
// chaining its own dial through one: streamSettings.sockopt.dialerProxy →
// a freedom outbound carrying settings.fragment.
//
// For a master entry its own transport already rides the pool
// (sockopt.dialerProxy is the dialer chain), and fragmenting a hello
// inside an encrypted tunnel hides nothing. What DPI sees is each pool
// node's connection, so a master's fragment is applied to its slot's
// member outbounds instead. A slot is shared by masters with the same
// Dialer refs; it fragments when any of them asks, with the settings of
// the first such master by name.
//
// Freedom outbounds are content-addressed (FragmentOutboundTag): entries
// with identical settings share one, and changing the settings changes
// the tag, which changes the entry's own outbound — so a live apply
// replaces it and the stale-path sweeper sees it like any other edit.

// Fragment is the freedom outbound's settings.fragment block.
//
//   - Packets: "tlshello" splits only the TLS ClientHello; "1-3" (or "1")
//     splits the first TCP writes, for transports without TLS.
//   - Length: bytes per fragment, "MIN-MAX" (random in range) or "N".
//   - Interval: milliseconds between fragments, "MIN-MAX" or "N"; 0 sends
//     them back to back as separate segments.
type Fragment struct {
	Packets  string `json:"packets"`
	Length   string `json:"length"`
	Interval string `json:"interval"`
}

// DefaultFragment is the widely used setting: split only the ClientHello,
// 100–200 byte pieces, 10–20 ms apart.
var DefaultFragment = Fragment{Packets: "tlshello", Length: "100-200", Interval: "10-20"}

// ErrInvalidFragment is returned for a fragment setting xray would reject.
var ErrInvalidFragment = errors.New("invalid fragment settings")

// ParseFragment decodes a stored fragment setting. ok is false for the
// empty string (fragment off) or anything that doesn't decode.
func ParseFragment(s string) (Fragment, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Fragment{}, false
	}
	var f Fragment
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return Fragment{}, false
	}
	return f, true
}

// Encode returns the canonical stored form.
func (f Fragment) Encode() string {
	b, _ := json.Marshal(f)
	return string(b)
}

// Normalize trims the fields, fills empty ones from DefaultFragment and
// validates the result.
func (f Fragment) Normalize() (Fragment, error) {
	f.Packets = strings.ToLower(strings.ReplaceAll(f.Packets, " ", ""))
	f.Length = strings.ReplaceAll(f.Length, " ", "")
	f.Interval = strings.ReplaceAll(f.Interval, " ", "")
	if f.Packets == "" {
		f.Packets = DefaultFragment.Packets
	}
	if f.Length == "" {
		f.Length = DefaultFragment.Length
	}
	if f.Interval == "" {
		f.Interval = DefaultFragment.Interval
	}
	if f.Packets != "tlshello" {
		if lo, _, ok := parseRange(f.Packets); !ok || lo < 1 {
			return f, fmtFragErr("packets must be \"tlshello\" or a packet range like \"1-3\"")
		}
	}
	if lo, _, ok := parseRange(f.Length); !ok || lo < 1 {
		return f, fmtFragErr("length must be a byte range like \"100-200\"")
	}
	if _, _, ok := parseRange(f.Interval); !ok {
		return f, fmtFragErr("interval must be a millisecond range like \"10-20\"")
	}
	return f, nil
}

func fmtFragErr(msg string) error { return errors.Join(ErrInvalidFragment, errors.New(msg)) }

// parseRange accepts "N" or "MIN-MAX" with 0 ≤ MIN ≤ MAX.
func parseRange(s string) (lo, hi int, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	lo, err := strconv.Atoi(a)
	if err != nil || lo < 0 {
		return 0, 0, false
	}
	hi = lo
	if isRange {
		if hi, err = strconv.Atoi(b); err != nil || hi < lo {
			return 0, 0, false
		}
	}
	return lo, hi, true
}

// FragmentOutboundTag is the freedom outbound carrying f. Content-addressed
// so identical settings share one outbound (see the file comment). Never
// starts with ObservatorySelectorPrefix: the observatory would probe it.
func FragmentOutboundTag(f Fragment) string {
	sum := sha256.Sum256([]byte(f.Encode()))
	return "frag-" + hex.EncodeToString(sum[:4])
}

// fragmentOutbound is the freedom outbound Generate emits for f.
func fragmentOutbound(f Fragment) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"tag":      FragmentOutboundTag(f),
		"protocol": "freedom",
		"settings": map[string]any{"fragment": f},
	})
	return raw
}

// FragmentSupport reports whether fragmentation can be applied to an
// entry's outbound, and if not, why — the reason is shown next to the
// toggle. A master is judged by its pool, not its own outbound.
func FragmentSupport(outbound string, master bool, f Fragment) (bool, string) {
	if master {
		return true, ""
	}
	var ob map[string]any
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		return false, "outbound JSON does not parse"
	}
	return fragmentSupport(ob, f)
}

func fragmentSupport(ob map[string]any, f Fragment) (bool, string) {
	proto, _ := ob["protocol"].(string)
	switch strings.ToLower(proto) {
	case "freedom", "blackhole", "dns", "loopback":
		return false, "a " + proto + " outbound has no server connection to fragment"
	case "wireguard", "hysteria":
		return false, proto + " runs over UDP; fragmentation is TCP only"
	}
	ss, _ := ob["streamSettings"].(map[string]any)
	if sock, _ := ss["sockopt"].(map[string]any); sock != nil {
		if dp, _ := sock["dialerProxy"].(string); strings.TrimSpace(dp) != "" {
			return false, "outbound JSON already sets its own sockopt.dialerProxy"
		}
	}
	network, _ := ss["network"].(string)
	switch strings.ToLower(network) {
	case "kcp", "mkcp", "quic", "hysteria":
		return false, "transport " + network + " runs over UDP; fragmentation is TCP only"
	}
	if f.Packets == "tlshello" {
		security, _ := ss["security"].(string)
		switch strings.ToLower(security) {
		case "tls", "reality":
		default:
			return false, "no TLS on this outbound, so there is no ClientHello to split — use a packet range like \"1-3\""
		}
	}
	return true, ""
}

// applyFragment chains ob's dial through the fragment outbound when ob
// supports it, and reports whether it did.
func applyFragment(ob map[string]any, f Fragment) bool {
	if ok, _ := fragmentSupport(ob, f); !ok {
		return false
	}
	injectDialerProxy(ob, FragmentOutboundTag(f))
	return true
}
