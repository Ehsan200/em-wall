package xray

import (
	"encoding/json"
	"strings"
)

// Connection multiplexing (xray mux.cool) for an entry's outbound.
//
// Without it every connection routed through an entry opens its own tunnel
// to the server — a fresh TCP + TLS + WebSocket (or similar) handshake over
// the user's uplink before the first byte flows, and for a master that
// handshake itself rides a pool node, doubling the round trips. With mux,
// connections become streams inside a few long-lived tunnels, so new ones
// start with no handshake at all.
//
// The trade-off is shared fate: a tunnel that breaks takes every stream on
// it down together, and one slow stream can delay its siblings. That is
// why it is opt-in per entry, and why it is only applied where it helps:
//
//   - transports that already multiplex (gRPC, XHTTP/SplitHTTP, HTTP/2,
//     QUIC) gain nothing from a second layer;
//   - VLESS with an XTLS flow (xtls-rprx-vision) is incompatible with mux;
//   - only VMess, VLESS and Trojan are enabled — the protocols xray's mux
//     is documented and widely deployed for. The server needs nothing: an
//     xray inbound accepts mux streams on its own.
//
// An outbound whose JSON already carries a "mux" block is left exactly as
// the user wrote it.

// muxSettings is what Generate injects. UDP/443 (QUIC) is kept off the mux
// ("skip") so enabling mux never changes how QUIC reaches the entry.
var muxSettings = map[string]any{
	"enabled":         true,
	"concurrency":     8,
	"xudpConcurrency": 16,
	"xudpProxyUDP443": "skip",
}

// MuxSupport reports whether mux can be applied to an outbound, and if not,
// why — the reason is shown next to the toggle.
func MuxSupport(outbound string) (bool, string) {
	var ob map[string]any
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		return false, "outbound JSON does not parse"
	}
	return muxSupport(ob)
}

func muxSupport(ob map[string]any) (bool, string) {
	if _, ok := ob["mux"]; ok {
		return false, "outbound JSON already sets its own mux"
	}
	proto, _ := ob["protocol"].(string)
	switch strings.ToLower(proto) {
	case "vmess", "trojan":
	case "vless":
		if hasXTLSFlow(ob) {
			return false, "XTLS flow (vision) cannot be multiplexed"
		}
	default:
		return false, "only VMess, VLESS and Trojan support multiplexing"
	}
	ss, _ := ob["streamSettings"].(map[string]any)
	network, _ := ss["network"].(string)
	switch strings.ToLower(network) {
	case "grpc", "xhttp", "splithttp", "h2", "http", "quic":
		return false, "transport " + network + " already multiplexes"
	}
	return true, ""
}

// hasXTLSFlow reports a VLESS outbound using an XTLS flow on any user.
func hasXTLSFlow(ob map[string]any) bool {
	st, _ := ob["settings"].(map[string]any)
	vnext, _ := st["vnext"].([]any)
	for _, v := range vnext {
		srv, _ := v.(map[string]any)
		users, _ := srv["users"].([]any)
		for _, u := range users {
			um, _ := u.(map[string]any)
			if flow, _ := um["flow"].(string); strings.TrimSpace(flow) != "" {
				return true
			}
		}
	}
	// Flat form ("address"/"id"/"flow" directly in settings).
	if flow, _ := st["flow"].(string); strings.TrimSpace(flow) != "" {
		return true
	}
	return false
}

// applyMux injects the mux block when the outbound supports it; otherwise
// it leaves the outbound untouched.
func applyMux(ob map[string]any) {
	if ok, _ := muxSupport(ob); ok {
		m := make(map[string]any, len(muxSettings))
		for k, v := range muxSettings {
			m[k] = v
		}
		ob["mux"] = m
	}
}
