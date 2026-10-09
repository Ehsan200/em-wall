package xray

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// BuildLink is the inverse of ParseLink: it turns one stored outbound
// JSON block into a share URI ("vless://", "vmess://", "trojan://",
// "ss://") that V2Box/v2rayN/Hiddify — or ParseLink — can import. name
// becomes the link's display name.
//
// A share link only describes one server and its transport, so anything
// outside that is lost; notes lists what was left out (an empty list
// means the link carries the whole outbound). The caller adds its own
// notes for entry-level settings (dialer, mux, fragment) that never live
// in the outbound JSON.
func BuildLink(name, outbound string) (link string, notes []string, err error) {
	var ob map[string]any
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		return "", nil, fmt.Errorf("xray: outbound JSON: %w", err)
	}
	proto, _ := ob["protocol"].(string)
	proto = strings.ToLower(strings.TrimSpace(proto))
	normalizeFlatSettings(ob, proto)
	settings, _ := ob["settings"].(map[string]any)
	ss, _ := ob["streamSettings"].(map[string]any)

	if _, ok := ob["mux"]; ok {
		notes = append(notes, "the outbound's own mux block")
	}
	if ss != nil {
		if _, ok := ss["sockopt"]; ok {
			notes = append(notes, "streamSettings.sockopt")
		}
	}

	switch proto {
	case "vless":
		srv, user, n, err := firstVnextUser(settings)
		if err != nil {
			return "", nil, fmt.Errorf("xray vless: %w", err)
		}
		notes = append(notes, n...)
		id := strAny(user["id"])
		if id == "" {
			return "", nil, fmt.Errorf("xray vless: missing user id")
		}
		q, sn := streamQuery(ss)
		notes = append(notes, sn...)
		q.Set("encryption", firstNonEmpty(strAny(user["encryption"]), "none"))
		if flow := strAny(user["flow"]); flow != "" {
			q.Set("flow", flow)
		}
		return uriLink("vless", url.User(id), srv, q, name), notes, nil

	case "trojan":
		srv, n, err := firstServer(settings)
		if err != nil {
			return "", nil, fmt.Errorf("xray trojan: %w", err)
		}
		notes = append(notes, n...)
		pw := strAny(srv["password"])
		if pw == "" {
			return "", nil, fmt.Errorf("xray trojan: missing password")
		}
		q, sn := streamQuery(ss)
		notes = append(notes, sn...)
		// ParseLink (and most clients) read a missing security as TLS.
		if q.Get("security") == "" {
			q.Set("security", "none")
		}
		return uriLink("trojan", url.User(pw), srv, q, name), notes, nil

	case "shadowsocks":
		srv, n, err := firstServer(settings)
		if err != nil {
			return "", nil, fmt.Errorf("xray shadowsocks: %w", err)
		}
		notes = append(notes, n...)
		method, pw := strAny(srv["method"]), strAny(srv["password"])
		if method == "" || pw == "" {
			return "", nil, fmt.Errorf("xray shadowsocks: missing method/password")
		}
		if ss != nil {
			if nw := strAny(ss["network"]); nw != "" && nw != "tcp" {
				notes = append(notes, "the "+nw+" transport (ss links carry none)")
			}
			if sec := strAny(ss["security"]); sec != "" && sec != "none" {
				notes = append(notes, sec+" security (ss links carry none)")
			}
		}
		// SIP002: base64url(method:password) as the userinfo.
		ui := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + pw))
		return uriLink("ss", url.User(ui), srv, nil, name), notes, nil

	case "vmess":
		srv, user, n, err := firstVnextUser(settings)
		if err != nil {
			return "", nil, fmt.Errorf("xray vmess: %w", err)
		}
		notes = append(notes, n...)
		id := strAny(user["id"])
		if id == "" {
			return "", nil, fmt.Errorf("xray vmess: missing user id")
		}
		port, _ := toIntLoose(srv["port"])
		q, sn := streamQuery(ss)
		notes = append(notes, sn...)
		aid, _ := toIntLoose(user["alterId"])
		// v2rayN's vmess JSON; ports and alterId travel as strings.
		v := map[string]string{
			"v":    "2",
			"ps":   name,
			"add":  strAny(srv["address"]),
			"port": strconv.Itoa(port),
			"id":   id,
			"aid":  strconv.Itoa(aid),
			"scy":  firstNonEmpty(strAny(user["security"]), "auto"),
			"net":  q.Get("type"),
			"type": firstNonEmpty(q.Get("headerType"), "none"),
			"host": q.Get("host"),
			"path": firstNonEmpty(q.Get("path"), q.Get("serviceName")),
			"tls":  q.Get("security"),
			"sni":  q.Get("sni"),
			"alpn": q.Get("alpn"),
			"fp":   q.Get("fp"),
		}
		if q.Get("security") == "reality" {
			v["tls"] = ""
			notes = append(notes, "REALITY settings (vmess links can't carry them)")
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", nil, err
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(b), notes, nil

	case "":
		return "", nil, fmt.Errorf("xray: outbound JSON missing \"protocol\"")
	default:
		return "", nil, fmt.Errorf("xray: no share-link format for protocol %q (vless, vmess, trojan and shadowsocks have one)", proto)
	}
}

// uriLink assembles SCHEME://USERINFO@HOST:PORT?QUERY#NAME. Port 0 (an
// outbound without one) still yields a link so the error surfaces on import.
func uriLink(scheme string, user *url.Userinfo, srv map[string]any, q url.Values, name string) string {
	port, _ := toIntLoose(srv["port"])
	u := url.URL{
		Scheme:   scheme,
		User:     user,
		Host:     net.JoinHostPort(strAny(srv["address"]), strconv.Itoa(port)),
		Fragment: name,
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// firstVnextUser picks settings.vnext[0].users[0]; extra servers and
// users can't go into one link and are reported in notes.
func firstVnextUser(settings map[string]any) (srv, user map[string]any, notes []string, err error) {
	vnext, _ := settings["vnext"].([]any)
	if len(vnext) == 0 {
		return nil, nil, nil, fmt.Errorf("settings.vnext is empty")
	}
	srv, _ = vnext[0].(map[string]any)
	if srv == nil || strAny(srv["address"]) == "" {
		return nil, nil, nil, fmt.Errorf("settings.vnext[0] has no address")
	}
	users, _ := srv["users"].([]any)
	if len(users) == 0 {
		return nil, nil, nil, fmt.Errorf("settings.vnext[0].users is empty")
	}
	user, _ = users[0].(map[string]any)
	if user == nil {
		return nil, nil, nil, fmt.Errorf("settings.vnext[0].users[0] is not an object")
	}
	if len(vnext) > 1 {
		notes = append(notes, fmt.Sprintf("%d more servers (only the first is linked)", len(vnext)-1))
	}
	if len(users) > 1 {
		notes = append(notes, fmt.Sprintf("%d more users (only the first is linked)", len(users)-1))
	}
	return srv, user, notes, nil
}

// firstServer picks settings.servers[0] (trojan, shadowsocks).
func firstServer(settings map[string]any) (map[string]any, []string, error) {
	servers, _ := settings["servers"].([]any)
	if len(servers) == 0 {
		return nil, nil, fmt.Errorf("settings.servers is empty")
	}
	srv, _ := servers[0].(map[string]any)
	if srv == nil || strAny(srv["address"]) == "" {
		return nil, nil, fmt.Errorf("settings.servers[0] has no address")
	}
	var notes []string
	if len(servers) > 1 {
		notes = append(notes, fmt.Sprintf("%d more servers (only the first is linked)", len(servers)-1))
	}
	return srv, notes, nil
}

// streamQuery flattens streamSettings into the share-link query
// convention — the exact inverse of buildStreamSettings.
func streamQuery(ss map[string]any) (url.Values, []string) {
	q := url.Values{}
	var notes []string
	network := firstNonEmpty(strAny(ss["network"]), "tcp")
	if network == "splithttp" {
		network = "xhttp"
	}
	q.Set("type", network)

	switch sec := strAny(ss["security"]); sec {
	case "", "none":
	case "tls":
		q.Set("security", "tls")
		tls, _ := ss["tlsSettings"].(map[string]any)
		setIf(q, "sni", strAny(tls["serverName"]))
		setIf(q, "fp", strAny(tls["fingerprint"]))
		setIf(q, "alpn", joinAny(tls["alpn"]))
		if b, _ := tls["allowInsecure"].(bool); b {
			q.Set("allowInsecure", "1")
		}
	case "reality":
		q.Set("security", "reality")
		r, _ := ss["realitySettings"].(map[string]any)
		setIf(q, "sni", strAny(r["serverName"]))
		setIf(q, "pbk", strAny(r["publicKey"]))
		setIf(q, "sid", strAny(r["shortId"]))
		setIf(q, "fp", strAny(r["fingerprint"]))
		setIf(q, "spx", strAny(r["spiderX"]))
	default:
		q.Set("security", sec)
		notes = append(notes, sec+" security settings")
	}

	switch network {
	case "tcp", "raw":
		tcp, _ := firstMap(ss, "tcpSettings", "rawSettings")
		header, _ := tcp["header"].(map[string]any)
		if strAny(header["type"]) == "http" {
			q.Set("headerType", "http")
			req, _ := header["request"].(map[string]any)
			setIf(q, "path", joinAny(req["path"]))
			if hs, ok := req["headers"].(map[string]any); ok {
				setIf(q, "host", joinAny(hs["Host"]))
			}
		}
	case "ws":
		ws, _ := ss["wsSettings"].(map[string]any)
		setIf(q, "path", strAny(ws["path"]))
		host := strAny(ws["host"])
		if hs, ok := ws["headers"].(map[string]any); ok && host == "" {
			host = joinAny(hs["Host"])
		}
		setIf(q, "host", host)
	case "grpc":
		g, _ := ss["grpcSettings"].(map[string]any)
		setIf(q, "serviceName", strAny(g["serviceName"]))
		setIf(q, "authority", strAny(g["authority"]))
		if b, _ := g["multiMode"].(bool); b {
			q.Set("mode", "multi")
		}
	case "h2", "http":
		h, _ := ss["httpSettings"].(map[string]any)
		setIf(q, "host", joinAny(h["host"]))
		setIf(q, "path", strAny(h["path"]))
	case "httpupgrade":
		h, _ := ss["httpupgradeSettings"].(map[string]any)
		setIf(q, "path", strAny(h["path"]))
		setIf(q, "host", strAny(h["host"]))
	case "xhttp":
		x, _ := firstMap(ss, "xhttpSettings", "splithttpSettings")
		setIf(q, "path", strAny(x["path"]))
		setIf(q, "host", strAny(x["host"]))
		setIf(q, "mode", strAny(x["mode"]))
		if extra, ok := x["extra"].(map[string]any); ok && len(extra) > 0 {
			if b, err := json.Marshal(extra); err == nil {
				q.Set("extra", string(b))
			}
		}
	default:
		notes = append(notes, network+" transport settings")
	}
	return q, notes
}

func firstMap(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v, true
		}
	}
	return nil, false
}

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

// strAny reads a JSON string or number as a string ("" otherwise).
func strAny(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return ""
}

// joinAny reads a string or an array of strings, comma-joined.
func joinAny(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		parts := make([]string, 0, len(s))
		for _, p := range s {
			if ps := strAny(p); ps != "" {
				parts = append(parts, ps)
			}
		}
		return strings.Join(parts, ",")
	}
	return ""
}
