// Package ipc is the wire protocol between the Wails UI and the daemon.
// Newline-framed JSON over a Unix socket. Each line is one Request or
// one Response.
package ipc

import "encoding/json"

const (
	DefaultSocketPath = "/var/run/em-wall.sock"
)

type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorBody      `json:"error,omitempty"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Method names. Keep this list as the single source of truth.
const (
	MethodStatus           = "status"
	MethodRulesList        = "rules.list"
	MethodRulesAdd         = "rules.add"
	MethodRulesUpdate      = "rules.update"
	MethodRulesDelete      = "rules.delete"
	MethodSettingsGet      = "settings.get"
	MethodSettingsSet      = "settings.set"
	MethodLogsRecent       = "logs.recent"
	MethodRoutesActive     = "routes.active"
	MethodInterfacesList   = "interfaces.list"
	MethodReload           = "reload"
	MethodSystemDNSStatus     = "system.dns.status"
	MethodSystemDNSActivate   = "system.dns.activate"
	MethodSystemDNSDeactivate = "system.dns.deactivate"
	MethodSystemRoutesList    = "system.routes.list"
)

// Param/result payloads. Plain structs, json-tagged.

type StatusResult struct {
	Version           string `json:"version"`
	Uptime            string `json:"uptime"`
	BlockEncryptedDNS bool   `json:"blockEncryptedDns"`
	UpstreamDNS       string `json:"upstreamDns"`
	ListenAddr        string `json:"listenAddr"`
	RuleCount         int    `json:"ruleCount"`
}

type RuleDTO struct {
	ID        int64  `json:"id"`
	Pattern   string `json:"pattern"`
	Action    string `json:"action"` // "block" or "allow"
	Interface string `json:"interface"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type RulesAddParams struct {
	Pattern   string `json:"pattern"`
	Action    string `json:"action"`
	Interface string `json:"interface"`
	Enabled   bool   `json:"enabled"`
}

type RulesUpdateParams struct {
	ID        int64  `json:"id"`
	Pattern   string `json:"pattern"`
	Action    string `json:"action"`
	Interface string `json:"interface"`
	Enabled   bool   `json:"enabled"`
}

type RulesDeleteParams struct {
	ID int64 `json:"id"`
}

type SettingsGetParams struct {
	Key     string `json:"key"`
	Default string `json:"default"`
}

type SettingsSetParams struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type LogsRecentParams struct {
	Limit int `json:"limit"`
}

type LogDTO struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"timestamp"`
	QueryName string `json:"queryName"`
	Action    string `json:"action"`
	RuleID    int64  `json:"ruleId"`
	Interface string `json:"interface"`
	ClientIP  string `json:"clientIp"`
}

type ActiveRouteDTO struct {
	Host      string `json:"host"`
	Interface string `json:"interface"`
	ExpiresAt string `json:"expiresAt"`
	RuleID    int64  `json:"ruleId"`
}

type InterfaceDTO struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
	MTU   int    `json:"mtu"`
	Flags string `json:"flags"`
	Owner string `json:"owner"` // best-effort VPN/app label
}

type SystemRouteDTO struct {
	Family      string `json:"family"`
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
	Flags       string `json:"flags"`
	Interface   string `json:"interface"`
}

type SystemDNSStatus struct {
	Active            bool                `json:"active"`              // any service has 127.0.0.1
	Upstream          []string            `json:"upstream"`            // current daemon upstream
	DetectedResolvers []string            `json:"detectedResolvers"`   // what scutil sees (excl. loopback)
	PerService        map[string][]string `json:"perService"`          // current per-service DNS
}
