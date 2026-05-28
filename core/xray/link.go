package xray

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseLink converts a share URI ("vless://...", "vmess://...",
// "trojan://...", "ss://...") into the JSON for a single xray
// outbound block plus a suggested entry name (derived from the URI
// fragment, lower-cased and constrained to ValidName's charset).
//
// The outbound JSON is what the user would otherwise hand-write in
// the editor — the supervisor still overwrites the "tag" field at
// generation time so any tag set here is harmless.
//
// Unsupported schemes or malformed URIs return a descriptive error;
// the UI shows it verbatim.
func ParseLink(raw string) (name string, outbound string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("xray: empty input")
	}
	// Two accepted shapes: a URI share-link (handled per-scheme below)
	// or a raw JSON outbound block pasted from another panel.
	if strings.HasPrefix(raw, "{") {
		return parseOutboundJSON(raw)
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", "", fmt.Errorf("xray: not a share URI and not JSON (need either \"<scheme>://\" or a JSON object starting with '{')")
	}
	switch strings.ToLower(scheme) {
	case "vless":
		return parseVLESS(rest)
	case "vmess":
		return parseVMESS(rest)
	case "trojan":
		return parseTrojan(rest)
	case "ss":
		return parseSS(rest)
	default:
		return "", "", fmt.Errorf("xray: unsupported scheme %q (want vless, vmess, trojan, ss)", scheme)
	}
}

// parseOutboundJSON accepts a raw xray outbound block. Panels like
// 3X-UI export outbounds with a FLAT settings shape (address/port/id
// at the top of settings) instead of the canonical vnext/servers
// wrapper xray-core needs — this function reshapes the flat form
// into canonical so a copy-paste actually starts xray.
//
// Inputs that are already canonical (settings.vnext or settings.servers
// present) are passed through unchanged.
//
// The suggested entry name comes from the JSON's "tag" field when
// present, sanitised by suggestName.
func parseOutboundJSON(raw string) (string, string, error) {
	var ob map[string]any
	if err := json.Unmarshal([]byte(raw), &ob); err != nil {
		return "", "", fmt.Errorf("xray: outbound JSON: %w", err)
	}
	proto, _ := ob["protocol"].(string)
	if strings.TrimSpace(proto) == "" {
		return "", "", fmt.Errorf("xray: outbound JSON missing \"protocol\"")
	}
	normalizeFlatSettings(ob, proto)

	name := "imported"
	if tag, _ := ob["tag"].(string); tag != "" {
		name = suggestName(tag)
	}
	out, err := marshalIndent(ob)
	if err != nil {
		return "", "", err
	}
	return name, out, nil
}

// normalizeFlatSettings rewrites a settings block that was exported
// in the flat "address at the top" form into the canonical wrapper
// xray-core's conf parser expects. No-op when settings is already
// canonical, or when there's no settings block at all.
//
// vless additionally gets a default settings.decryption = "none"
// (xray requires it; the flat form usually omits it).
func normalizeFlatSettings(ob map[string]any, protocol string) {
	settings, ok := ob["settings"].(map[string]any)
	if !ok {
		return
	}
	switch strings.ToLower(protocol) {
	case "vless":
		if _, has := settings["vnext"]; has {
			if _, hasDec := settings["decryption"]; !hasDec {
				settings["decryption"] = "none"
			}
			return
		}
		addr, _ := settings["address"].(string)
		port, _ := toIntLoose(settings["port"])
		if addr == "" || port == 0 {
			return
		}
		user := map[string]any{
			"id":         settings["id"],
			"encryption": firstNonEmptyAny(settings["encryption"], "none"),
		}
		copyIfPresent(user, settings, "flow")
		copyIfPresent(user, settings, "level")
		copyIfPresent(user, settings, "email")
		ob["settings"] = map[string]any{
			"vnext": []any{map[string]any{
				"address": addr,
				"port":    port,
				"users":   []any{user},
			}},
			"decryption": "none",
		}
	case "vmess":
		if _, has := settings["vnext"]; has {
			return
		}
		addr, _ := settings["address"].(string)
		port, _ := toIntLoose(settings["port"])
		if addr == "" || port == 0 {
			return
		}
		user := map[string]any{
			"id":       settings["id"],
			"security": firstNonEmptyAny(settings["security"], "auto"),
		}
		if a, ok := toIntLoose(settings["alterId"]); ok {
			user["alterId"] = a
		} else {
			user["alterId"] = 0
		}
		copyIfPresent(user, settings, "level")
		copyIfPresent(user, settings, "email")
		ob["settings"] = map[string]any{
			"vnext": []any{map[string]any{
				"address": addr,
				"port":    port,
				"users":   []any{user},
			}},
		}
	case "trojan":
		if _, has := settings["servers"]; has {
			return
		}
		addr, _ := settings["address"].(string)
		port, _ := toIntLoose(settings["port"])
		if addr == "" || port == 0 {
			return
		}
		server := map[string]any{
			"address":  addr,
			"port":     port,
			"password": settings["password"],
		}
		copyIfPresent(server, settings, "email")
		copyIfPresent(server, settings, "level")
		ob["settings"] = map[string]any{"servers": []any{server}}
	case "shadowsocks":
		if _, has := settings["servers"]; has {
			return
		}
		addr, _ := settings["address"].(string)
		port, _ := toIntLoose(settings["port"])
		if addr == "" || port == 0 {
			return
		}
		server := map[string]any{
			"address":  addr,
			"port":     port,
			"method":   settings["method"],
			"password": settings["password"],
		}
		copyIfPresent(server, settings, "email")
		copyIfPresent(server, settings, "level")
		ob["settings"] = map[string]any{"servers": []any{server}}
	}
}

func copyIfPresent(dst, src map[string]any, key string) {
	if v, ok := src[key]; ok {
		dst[key] = v
	}
}

func firstNonEmptyAny(v any, fallback string) any {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func toIntLoose(v any) (int, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	}
	return 0, false
}

// suggestName turns a URI fragment ("My Server #1") into a valid
// entry name ("my-server-1"). Empty fragments produce "imported".
func suggestName(frag string) string {
	frag = strings.TrimSpace(frag)
	if frag == "" {
		return "imported"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(frag) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ', r == '.', r == '/', r == '+':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "imported"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// marshalIndent serializes v with two-space indent — same shape as
// what the user would hand-write, so a round-trip through the editor
// doesn't show a diff.
func marshalIndent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---- VLESS ----
//
//	vless://UUID@HOST:PORT?<params>#NAME
func parseVLESS(rest string) (string, string, error) {
	u, err := url.Parse("vless://" + rest)
	if err != nil {
		return "", "", fmt.Errorf("xray vless: %w", err)
	}
	uuid := u.User.Username()
	if uuid == "" {
		return "", "", fmt.Errorf("xray vless: missing uuid")
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return "", "", fmt.Errorf("xray vless: bad host:port %q", u.Host)
	}
	q := u.Query()

	user := map[string]any{
		"id":         uuid,
		"encryption": firstNonEmpty(q.Get("encryption"), "none"),
	}
	if flow := q.Get("flow"); flow != "" {
		user["flow"] = flow
	}

	ob := map[string]any{
		"protocol": "vless",
		"settings": map[string]any{
			"vnext": []any{map[string]any{
				"address": host,
				"port":    port,
				"users":   []any{user},
			}},
		},
		"streamSettings": buildStreamSettings(q, host),
	}
	out, err := marshalIndent(ob)
	if err != nil {
		return "", "", err
	}
	return suggestName(u.Fragment), out, nil
}

// ---- VMESS ----
//
//	vmess://BASE64(JSON)
//
// The JSON has the v2rayN/x-ui field names:
//
//	{v, ps, add, port, id, aid, scy, net, type, host, path, tls, sni, alpn}
func parseVMESS(rest string) (string, string, error) {
	// vmess links are pure base64 (sometimes URL-safe), no query/path.
	dec, err := decodeBase64Flexible(rest)
	if err != nil {
		return "", "", fmt.Errorf("xray vmess: base64: %w", err)
	}
	var v struct {
		PS   string `json:"ps"`
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		Aid  any    `json:"aid"`
		Scy  string `json:"scy"`
		Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
		ALPN string `json:"alpn"`
		FP   string `json:"fp"`
	}
	if err := json.Unmarshal(dec, &v); err != nil {
		return "", "", fmt.Errorf("xray vmess: decode payload: %w", err)
	}
	if v.Add == "" || v.ID == "" {
		return "", "", fmt.Errorf("xray vmess: missing add/id")
	}
	port, err := toInt(v.Port)
	if err != nil {
		return "", "", fmt.Errorf("xray vmess: port: %w", err)
	}
	aid, _ := toInt(v.Aid)
	scy := firstNonEmpty(v.Scy, "auto")

	user := map[string]any{
		"id":       v.ID,
		"alterId":  aid,
		"security": scy,
	}

	// Build streamSettings from the loose vmess field set using the
	// same shape as buildStreamSettings but with vmess's quirks.
	q := url.Values{}
	if v.Net != "" {
		q.Set("type", v.Net)
	}
	if v.TLS != "" {
		q.Set("security", v.TLS)
	}
	if v.SNI != "" {
		q.Set("sni", v.SNI)
	}
	if v.Host != "" {
		q.Set("host", v.Host)
	}
	if v.Path != "" {
		q.Set("path", v.Path)
	}
	if v.ALPN != "" {
		q.Set("alpn", v.ALPN)
	}
	if v.FP != "" {
		q.Set("fp", v.FP)
	}

	ob := map[string]any{
		"protocol": "vmess",
		"settings": map[string]any{
			"vnext": []any{map[string]any{
				"address": v.Add,
				"port":    port,
				"users":   []any{user},
			}},
		},
		"streamSettings": buildStreamSettings(q, v.Add),
	}
	out, err := marshalIndent(ob)
	if err != nil {
		return "", "", err
	}
	return suggestName(v.PS), out, nil
}

// ---- Trojan ----
//
//	trojan://PASSWORD@HOST:PORT?<params>#NAME
func parseTrojan(rest string) (string, string, error) {
	u, err := url.Parse("trojan://" + rest)
	if err != nil {
		return "", "", fmt.Errorf("xray trojan: %w", err)
	}
	pw := u.User.Username()
	if pw == "" {
		return "", "", fmt.Errorf("xray trojan: missing password")
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" {
		return "", "", fmt.Errorf("xray trojan: bad host:port %q", u.Host)
	}
	q := u.Query()
	// Trojan implies TLS unless the user explicitly set security=none.
	if q.Get("security") == "" {
		q.Set("security", "tls")
	}

	ob := map[string]any{
		"protocol": "trojan",
		"settings": map[string]any{
			"servers": []any{map[string]any{
				"address":  host,
				"port":     port,
				"password": pw,
			}},
		},
		"streamSettings": buildStreamSettings(q, host),
	}
	out, err := marshalIndent(ob)
	if err != nil {
		return "", "", err
	}
	return suggestName(u.Fragment), out, nil
}

// ---- Shadowsocks ----
//
// Two formats coexist in the wild:
//
//	Legacy: ss://BASE64(METHOD:PASSWORD@HOST:PORT)#NAME
//	SIP002: ss://BASE64(METHOD:PASSWORD)@HOST:PORT?plugin=...#NAME
//
// We try SIP002 first (because the user info is base64'd separately
// and the URL parses cleanly), and fall back to legacy when that
// doesn't produce a host.
func parseSS(rest string) (string, string, error) {
	// Try SIP002 by parsing as a URL directly.
	u, err := url.Parse("ss://" + rest)
	if err == nil && u.Host != "" {
		method, password, perr := splitSSUserInfo(u.User.Username())
		if perr == nil {
			host := u.Hostname()
			port, perr := strconv.Atoi(u.Port())
			if perr == nil && host != "" {
				return buildSSOutbound(method, password, host, port, u.Fragment)
			}
		}
	}
	// Legacy: BASE64(METHOD:PASSWORD@HOST:PORT)[#NAME]
	frag := ""
	body := rest
	if i := strings.Index(body, "#"); i >= 0 {
		frag, _ = url.QueryUnescape(body[i+1:])
		body = body[:i]
	}
	dec, derr := decodeBase64Flexible(body)
	if derr != nil {
		return "", "", fmt.Errorf("xray ss: not SIP002 and legacy base64 failed: %w", derr)
	}
	at := strings.LastIndex(string(dec), "@")
	if at < 0 {
		return "", "", fmt.Errorf("xray ss: legacy payload missing @")
	}
	method, password, perr := splitSSUserInfo(string(dec[:at]))
	if perr != nil {
		return "", "", fmt.Errorf("xray ss: %w", perr)
	}
	hostport := string(dec[at+1:])
	host, portStr, ok := strings.Cut(hostport, ":")
	if !ok {
		return "", "", fmt.Errorf("xray ss: legacy host:port malformed")
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		return "", "", fmt.Errorf("xray ss: legacy port: %w", perr)
	}
	return buildSSOutbound(method, password, host, port, frag)
}

func splitSSUserInfo(s string) (method, password string, err error) {
	// user info is sometimes base64 (SIP002 with no plugin) and
	// sometimes plain (when the URL encoder already decoded it). Try
	// both, in that order.
	if dec, derr := decodeBase64Flexible(s); derr == nil && strings.Contains(string(dec), ":") {
		s = string(dec)
	}
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("user info has no ':'")
	}
	return s[:i], s[i+1:], nil
}

func buildSSOutbound(method, password, host string, port int, frag string) (string, string, error) {
	ob := map[string]any{
		"protocol": "shadowsocks",
		"settings": map[string]any{
			"servers": []any{map[string]any{
				"address":  host,
				"port":     port,
				"method":   method,
				"password": password,
			}},
		},
	}
	out, err := marshalIndent(ob)
	if err != nil {
		return "", "", err
	}
	return suggestName(frag), out, nil
}

// ---- shared helpers ----

// buildStreamSettings translates a flat URL-query view of transport
// params (the convention v2rayN/x-ui share links use) into the nested
// xray streamSettings object. defaultSNI fills in SNI when neither
// "sni" nor "host" is set.
func buildStreamSettings(q url.Values, defaultSNI string) map[string]any {
	net := firstNonEmpty(q.Get("type"), "tcp")
	sec := q.Get("security") // "", "tls", "reality"

	ss := map[string]any{"network": net}
	if sec != "" {
		ss["security"] = sec
	}

	switch sec {
	case "tls":
		tls := map[string]any{}
		sni := firstNonEmpty(q.Get("sni"), q.Get("host"), defaultSNI)
		if sni != "" {
			tls["serverName"] = sni
		}
		if fp := q.Get("fp"); fp != "" {
			tls["fingerprint"] = fp
		}
		if alpn := q.Get("alpn"); alpn != "" {
			tls["alpn"] = strings.Split(alpn, ",")
		}
		ss["tlsSettings"] = tls
	case "reality":
		reality := map[string]any{}
		if sni := firstNonEmpty(q.Get("sni"), defaultSNI); sni != "" {
			reality["serverName"] = sni
		}
		if pbk := q.Get("pbk"); pbk != "" {
			reality["publicKey"] = pbk
		}
		if sid := q.Get("sid"); sid != "" {
			reality["shortId"] = sid
		}
		if fp := q.Get("fp"); fp != "" {
			reality["fingerprint"] = fp
		}
		if spx := q.Get("spx"); spx != "" {
			reality["spiderX"] = spx
		}
		ss["realitySettings"] = reality
	}

	// Older share links say "splithttp" for what newer xray-core
	// calls "xhttp" — normalize before the per-network switch so the
	// rest of this function only has to know one name.
	if net == "splithttp" {
		net = "xhttp"
		ss["network"] = "xhttp"
	}

	switch net {
	case "ws":
		ws := map[string]any{
			"path": firstNonEmpty(q.Get("path"), "/"),
		}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		ss["wsSettings"] = ws
	case "grpc":
		grpc := map[string]any{}
		if sn := firstNonEmpty(q.Get("serviceName"), q.Get("path")); sn != "" {
			grpc["serviceName"] = strings.TrimPrefix(sn, "/")
		}
		ss["grpcSettings"] = grpc
	case "h2", "http":
		h2 := map[string]any{}
		if h := q.Get("host"); h != "" {
			h2["host"] = strings.Split(h, ",")
		}
		if p := q.Get("path"); p != "" {
			h2["path"] = p
		}
		ss["httpSettings"] = h2
	case "tcp":
		if t := q.Get("headerType"); t == "http" {
			ss["tcpSettings"] = map[string]any{
				"header": map[string]any{"type": "http"},
			}
		}
	case "xhttp":
		// xhttp (formerly splithttp) — xray-core's HTTP/H2/H3-aware
		// transport. The current xray release uses xhttpSettings; the
		// fields it accepts are a subset of:
		//   {host, path, mode, extra}
		// "mode" enumerates packet-up | stream-up | stream-one | auto.
		x := map[string]any{
			"path": firstNonEmpty(q.Get("path"), "/"),
		}
		if h := q.Get("host"); h != "" {
			x["host"] = h
		}
		if m := q.Get("mode"); m != "" {
			x["mode"] = m
		}
		// "extra" in xhttp share links is a URL-encoded JSON object —
		// xray-core deserializes it as conf.SplitHTTPConfig (NOT a
		// string). Decode it back into a map here; if it isn't valid
		// JSON, drop it (a bad blob would fail xray's startup, which
		// is worse than just omitting the optional field).
		if e := q.Get("extra"); e != "" {
			var extra map[string]any
			if err := json.Unmarshal([]byte(e), &extra); err == nil {
				x["extra"] = extra
			}
		}
		ss["xhttpSettings"] = x
	}

	return ss
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case string:
		if n == "" {
			return 0, nil
		}
		return strconv.Atoi(n)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

// decodeBase64Flexible decodes either standard or URL-safe base64,
// with or without padding — share links arrive in all four shapes.
func decodeBase64Flexible(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	enc := base64.StdEncoding.WithPadding(base64.NoPadding)
	if strings.ContainsAny(s, "-_") {
		enc = base64.URLEncoding.WithPadding(base64.NoPadding)
	}
	return enc.DecodeString(s)
}
