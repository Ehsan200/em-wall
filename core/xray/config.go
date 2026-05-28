package xray

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GenerateOptions tunes the composite config built by Generate.
//
//   - LogDir, when non-empty, is where xray will write its access/error
//     logs (XRAY_LOCATION_ASSET-style — both files end up under here).
//   - RoutingRules, when non-empty, is a raw JSON ARRAY of routing-rule
//     objects (must pass ValidateRoutingRules upstream). The user
//     supplies it via the Xray panel's routing editor. These rules are
//     PREPENDED to the per-entry inbound→outbound pairs so they take
//     precedence (xray evaluates rules top-to-bottom; first match wins).
type GenerateOptions struct {
	LogDir       string
	RoutingRules string
}

// Generate produces a full xray-core JSON config from entries.
//
// For each enabled entry the config gets:
//   - one SOCKS5 inbound bound to 127.0.0.1:SocksPort, tag "in-NAME"
//   - the user's outbound JSON with tag forced to "out-NAME"
//   - a routing rule pinning in-NAME → out-NAME (appended after any
//     user rules so the user's geosite/geoip/etc. rules win first).
//
// Two extra outbounds are ALWAYS present so user routing rules have
// somewhere to send traffic: TagDirect (freedom protocol — passes
// through unchanged) and TagBlock (blackhole — drops the connection).
//
// Output is deterministic (entries sorted by name) so a no-op config
// reload doesn't write a churn-different file.
func Generate(entries []Config, opt GenerateOptions) ([]byte, error) {
	sorted := make([]Config, 0, len(entries))
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	type inbound struct {
		Tag      string         `json:"tag"`
		Listen   string         `json:"listen"`
		Port     int            `json:"port"`
		Protocol string         `json:"protocol"`
		Settings map[string]any `json:"settings"`
	}
	type cfg struct {
		Log       map[string]any    `json:"log,omitempty"`
		Inbounds  []inbound         `json:"inbounds"`
		Outbounds []json.RawMessage `json:"outbounds"`
		Routing   struct {
			DomainStrategy string            `json:"domainStrategy"`
			Rules          []json.RawMessage `json:"rules"`
		} `json:"routing"`
	}

	out := cfg{Inbounds: []inbound{}, Outbounds: []json.RawMessage{}}
	if opt.LogDir != "" {
		out.Log = map[string]any{
			"loglevel": "warning",
			"access":   opt.LogDir + "/em-wall-xray-access.log",
			"error":    opt.LogDir + "/em-wall-xray-error.log",
		}
	}
	out.Routing.DomainStrategy = "AsIs"

	// User rules first — they take precedence over per-entry pairs.
	if r := strings.TrimSpace(opt.RoutingRules); r != "" && r != "[]" {
		var userRules []json.RawMessage
		if err := json.Unmarshal([]byte(r), &userRules); err != nil {
			return nil, fmt.Errorf("xray: user routing rules: %w", err)
		}
		out.Routing.Rules = append(out.Routing.Rules, userRules...)
	}

	for _, e := range sorted {
		out.Inbounds = append(out.Inbounds, inbound{
			Tag:      InboundTag(e.Name),
			Listen:   "127.0.0.1",
			Port:     e.SocksPort,
			Protocol: "socks",
			Settings: map[string]any{
				"auth": "noauth",
				"udp":  true,
				"ip":   "127.0.0.1",
			},
		})

		var ob map[string]any
		if err := json.Unmarshal([]byte(e.Outbound), &ob); err != nil {
			return nil, fmt.Errorf("xray: entry %q: outbound JSON: %w", e.Name, err)
		}
		ob["tag"] = OutboundTag(e.Name)
		// Auto-heal a known-bad shape we used to ship: xhttp's "extra"
		// must be a JSON object (xray's conf.SplitHTTPConfig), but an
		// earlier link-import build stored it as a string. Decode it
		// back to an object here so legacy entries don't fail xray's
		// startup; if the string isn't valid JSON, drop the field so
		// xray at least starts (without that optional config).
		healXHTTPExtra(ob)
		rawOut, err := json.Marshal(ob)
		if err != nil {
			return nil, fmt.Errorf("xray: entry %q: re-marshal outbound: %w", e.Name, err)
		}
		out.Outbounds = append(out.Outbounds, rawOut)

		pair, _ := json.Marshal(map[string]any{
			"type":        "field",
			"inboundTag":  []string{InboundTag(e.Name)},
			"outboundTag": OutboundTag(e.Name),
		})
		out.Routing.Rules = append(out.Routing.Rules, pair)
	}

	// Always-present outbounds so user rules can reference them.
	out.Outbounds = append(out.Outbounds,
		json.RawMessage(`{"tag":"`+TagDirect+`","protocol":"freedom"}`),
		json.RawMessage(`{"tag":"`+TagBlock+`","protocol":"blackhole"}`),
	)

	return json.MarshalIndent(out, "", "  ")
}

// healXHTTPExtra mutates ob in place if it carries a stream-settings
// path of (.streamSettings.xhttpSettings.extra) whose value is a
// string. Older em-wall link-import builds stored that field as a
// raw string when the share link's "extra" param was a URL-encoded
// JSON blob; xray-core deserializes it as conf.SplitHTTPConfig and
// rejects a string with:
//
//	Failed to build XHTTP config. > Failed to unmarshal "extra".
//	> json: cannot unmarshal string into Go value of type conf.SplitHTTPConfig
//
// If the string parses as a JSON object we promote it to the object
// form; if it doesn't, we drop the field so xray at least starts.
func healXHTTPExtra(ob map[string]any) {
	ss, ok := ob["streamSettings"].(map[string]any)
	if !ok {
		return
	}
	xh, ok := ss["xhttpSettings"].(map[string]any)
	if !ok {
		return
	}
	switch e := xh["extra"].(type) {
	case string:
		if e == "" {
			delete(xh, "extra")
			return
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(e), &decoded) == nil {
			xh["extra"] = decoded
		} else {
			delete(xh, "extra")
		}
	}
}
