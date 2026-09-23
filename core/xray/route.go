package xray

import (
	"encoding/json"
	"strings"
)

// OutboundServerHost returns the server host an outbound connects to —
// settings.vnext[0].address (VMess/VLESS), settings.servers[0].address
// (Trojan/Shadowsocks/SOCKS/HTTP), or a flat settings.address — lowercased,
// or "" when there is none (freedom, blackhole, unparseable JSON).
func OutboundServerHost(outbound string) string {
	var ob struct {
		Settings struct {
			Vnext []struct {
				Address string `json:"address"`
			} `json:"vnext"`
			Servers []struct {
				Address string `json:"address"`
			} `json:"servers"`
			Address string `json:"address"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		return ""
	}
	var h string
	switch {
	case len(ob.Settings.Vnext) > 0:
		h = ob.Settings.Vnext[0].Address
	case len(ob.Settings.Servers) > 0:
		h = ob.Settings.Servers[0].Address
	default:
		h = ob.Settings.Address
	}
	return strings.ToLower(strings.TrimSpace(h))
}
