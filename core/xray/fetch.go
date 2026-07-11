package xray

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxSubBytes caps a subscription response body so a hostile or broken
// endpoint can't exhaust memory. Generous for thousands of links.
const maxSubBytes = 8 << 20 // 8 MiB

// SubUserInfo is the data-quota accounting a provider optionally reports
// via the Subscription-Userinfo response header, in the same shape v2rayN
// / v2box surface to the user. Byte counters; Expire is a Unix timestamp
// (0 = never / not reported). HasData is false when the header was absent
// so callers can distinguish "0 bytes used" from "provider said nothing".
type SubUserInfo struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"`
	HasData  bool  `json:"hasData"`
}

// ParseUserinfo parses a Subscription-Userinfo header value, e.g.
// "upload=455211520; download=4909002752; total=107374182400; expire=1596686400".
// Missing keys stay zero. ok is false when the header is empty or carries
// none of the known keys.
func ParseUserinfo(header string) (SubUserInfo, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return SubUserInfo{}, false
	}
	var info SubUserInfo
	var any bool
	for _, part := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kv[0])) {
		case "upload":
			info.Upload, any = n, true
		case "download":
			info.Download, any = n, true
		case "total":
			info.Total, any = n, true
		case "expire":
			info.Expire, any = n, true
		}
	}
	if !any {
		return SubUserInfo{}, false
	}
	info.HasData = true
	return info, true
}

// FetchSubscriptionBody performs the HTTP GET for a subscription URL and
// returns the raw body plus any parsed Subscription-Userinfo quota (info.
// HasData is false when the header was absent). The caller supplies the
// http.Client so the daemon can choose a direct transport or one that
// dials through a healthy node. ua falls back to DefaultSubUserAgent when
// empty.
func FetchSubscriptionBody(ctx context.Context, client *http.Client, rawURL, ua string) ([]byte, SubUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, SubUserInfo{}, fmt.Errorf("xray: subscription request: %w", err)
	}
	if strings.TrimSpace(ua) == "" {
		ua = DefaultSubUserAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil, SubUserInfo{}, fmt.Errorf("xray: subscription fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, SubUserInfo{}, fmt.Errorf("xray: subscription %s: HTTP %d", rawURL, resp.StatusCode)
	}
	info, _ := ParseUserinfo(resp.Header.Get("Subscription-Userinfo"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubBytes))
	if err != nil {
		return nil, SubUserInfo{}, fmt.Errorf("xray: subscription read: %w", err)
	}
	return body, info, nil
}

// DecodeSubscription turns a subscription body into its list of raw
// share-link lines. Providers return either the links in plaintext or,
// more commonly, one base64 blob encoding the newline-joined list. Both
// standard and URL-safe base64, padded or not, are accepted.
func DecodeSubscription(body []byte) []string {
	txt := strings.TrimSpace(string(body))
	if txt == "" {
		return nil
	}
	if looksLikeLinks(txt) {
		return splitLines(txt)
	}
	if dec, ok := tryBase64(txt); ok {
		return splitLines(dec)
	}
	// Not obviously base64 and no scheme detected — return lines as-is so
	// a caller still gets a chance to parse (bad lines are dropped later).
	return splitLines(txt)
}

// ParseSubscriptionBody decodes body and parses every share link into a
// SubNode. Bad or unsupported lines are skipped. Nodes are deduped by
// fingerprint (links differing only by name collapse to one); display
// names are prefixed with subName and de-duplicated with a numeric
// suffix. Returns ErrEmptySubscription when nothing parseable is found.
func ParseSubscriptionBody(body []byte, subName string) ([]SubNode, error) {
	lines := DecodeSubscription(body)
	seenFP := make(map[string]bool)
	seenName := make(map[string]int)
	var nodes []SubNode
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "//") {
			continue
		}
		frag, ob, err := ParseLink(ln)
		if err != nil {
			continue
		}
		fp := Fingerprint(ob)
		if seenFP[fp] {
			continue
		}
		seenFP[fp] = true

		name := nodeDisplayName(subName, frag, ob)
		if n, ok := seenName[name]; ok {
			seenName[name] = n + 1
			name = fmt.Sprintf("%s-%d", name, n+1)
		} else {
			seenName[name] = 1
		}

		nodes = append(nodes, SubNode{
			Name:          name,
			Fingerprint:   fp,
			Outbound:      ob,
			Active:        false,
			LastLatencyMs: -1,
		})
	}
	if len(nodes) == 0 {
		return nil, ErrEmptySubscription
	}
	return nodes, nil
}

// nodeDisplayName builds "subName/frag", falling back to the outbound's
// server host:port when the share-link fragment is empty.
func nodeDisplayName(subName, frag string, outbound string) string {
	frag = strings.TrimSpace(frag)
	if frag == "" {
		if hp := serverHostPort(outbound); hp != "" {
			frag = hp
		} else {
			frag = "node"
		}
	}
	subName = strings.TrimSpace(subName)
	if subName == "" {
		return frag
	}
	return subName + "/" + frag
}

// serverHostPort extracts "host:port" from an outbound JSON block,
// checking both the vnext (vless/vmess) and servers (trojan/ss) shapes.
// Returns "" when it can't be determined.
func serverHostPort(outbound string) string {
	var ob struct {
		Settings struct {
			Vnext []struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
			} `json:"vnext"`
			Servers []struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
			} `json:"servers"`
		} `json:"settings"`
	}
	if json.Unmarshal([]byte(outbound), &ob) != nil {
		return ""
	}
	if len(ob.Settings.Vnext) > 0 && ob.Settings.Vnext[0].Address != "" {
		return ob.Settings.Vnext[0].Address + ":" + strconv.Itoa(ob.Settings.Vnext[0].Port)
	}
	if len(ob.Settings.Servers) > 0 && ob.Settings.Servers[0].Address != "" {
		return ob.Settings.Servers[0].Address + ":" + strconv.Itoa(ob.Settings.Servers[0].Port)
	}
	return ""
}

// looksLikeLinks reports whether the first non-empty line already carries
// a share-link scheme, meaning the body is plaintext, not base64.
func looksLikeLinks(txt string) bool {
	for _, ln := range splitLines(txt) {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		i := strings.Index(ln, "://")
		if i <= 0 {
			return false
		}
		switch strings.ToLower(ln[:i]) {
		case "vless", "vmess", "trojan", "ss":
			return true
		}
		return false
	}
	return false
}

// tryBase64 attempts to decode txt (after stripping internal whitespace)
// as base64 in any of the four common encodings. ok is false if none
// yields valid bytes.
func tryBase64(txt string) (string, bool) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', ' ':
			return -1
		}
		return r
	}, txt)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if dec, err := enc.DecodeString(clean); err == nil && len(dec) > 0 {
			return string(dec), true
		}
	}
	return "", false
}

// splitLines splits on both \n and \r\n and drops empty trailing pieces.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}
