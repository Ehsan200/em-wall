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
	MethodStatus                 = "status"
	MethodRulesList              = "rules.list"
	MethodRulesAdd               = "rules.add"
	MethodRulesUpdate            = "rules.update"
	MethodRulesBulkUpdate        = "rules.bulk-update"
	MethodRulesDelete            = "rules.delete"
	MethodSettingsGet            = "settings.get"
	MethodSettingsSet            = "settings.set"
	MethodLogsRecent             = "logs.recent"
	MethodLogsClear              = "logs.clear"
	MethodRoutesActive           = "routes.active"
	MethodInterfacesList         = "interfaces.list"
	MethodReload                 = "reload"
	MethodSystemDNSStatus        = "system.dns.status"
	MethodSystemDNSActivate      = "system.dns.activate"
	MethodSystemDNSDeactivate    = "system.dns.deactivate"
	MethodSystemRoutesList       = "system.routes.list"
	MethodGroupsList             = "groups.list"
	MethodGroupsApply            = "groups.apply"
	MethodGroupsIcon             = "groups.icon"
	MethodGroupsDeleteRules      = "groups.delete-rules"
	MethodGroupsSetEnabled       = "groups.set-enabled"
	MethodGroupsAdd              = "groups.add"    // create a custom group def
	MethodGroupsUpdate           = "groups.update" // edit a custom group def
	MethodGroupsDelete           = "groups.delete"  // remove a custom group def
	MethodGroupsRefresh          = "groups.refresh" // re-fetch dynamic groups' vendor feeds
	MethodExport                 = "portable.export"
	MethodImport                 = "portable.import"
	MethodProxiesList            = "proxies.list"
	MethodProxiesAdd             = "proxies.add"
	MethodProxiesUpdate          = "proxies.update"
	MethodProxiesDelete          = "proxies.delete"
	MethodProxiesTest            = "proxies.test"
	MethodXrayList               = "xray.list"
	MethodXrayAdd                = "xray.add"
	MethodXrayUpdate             = "xray.update"
	MethodXrayDelete             = "xray.delete"
	MethodXraySetEnabled         = "xray.setEnabled"
	MethodXrayStatus             = "xray.status"
	MethodXrayGetRouting         = "xray.getRouting"
	MethodXraySetRouting         = "xray.setRouting"
	MethodXrayParseLink          = "xray.parseLink"
	MethodXrayTest               = "xray.test"
	MethodXrayObservatory        = "xray.observatory" // live balancer/winner status (raw `xray api bi`)
	MethodXraySubList            = "xraysub.list"
	MethodXraySubAdd             = "xraysub.add"
	MethodXraySubUpdate          = "xraysub.update"
	MethodXraySubDelete          = "xraysub.delete"
	MethodXraySubSetEnabled      = "xraysub.setEnabled"
	MethodXraySubRefresh         = "xraysub.refresh" // fetch now (ID, or 0 = all due)
	MethodXraySubNodes           = "xraysub.nodes"
	MethodXraySubSetNodeDisabled = "xraysub.setNodeDisabled"
	MethodPublicIP               = "net.public-ip"
	MethodRuleExitIP             = "rules.exit-ip"
	MethodUsageQuery             = "usage.query"
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

// RulesBulkUpdateParams retargets every rule in IDs to the same
// (Action, Interface) pair. Pattern and Enabled are intentionally not
// exposed — the bulk toolbar is for changing where rules route to, not
// for renaming or toggling them.
type RulesBulkUpdateParams struct {
	IDs       []int64 `json:"ids"`
	Action    string  `json:"action"`    // "block" or "route"
	Interface string  `json:"interface"` // empty for block, required for route
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
	Limit  int    `json:"limit"`
	Filter string `json:"filter"` // "", "route", "block", "unavailable", or exact action
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

type GroupDTO struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Patterns    []string `json:"patterns"`
	Color       string   `json:"color"`     // brand accent hex, "" if none
	Category    string   `json:"category"`  // display section, e.g. "AI"/"Developer"; "Custom" for user groups
	Custom      bool     `json:"custom"`    // true = user-created (editable/deletable)
	RuleCount   int      `json:"ruleCount"` // how many stored rules match this group's patterns
}

// DynamicGroupDTO reports one dynamic group's refresh outcome: how many
// prefixes its vendor feed now yields, and how many stored rules that
// changed. Error is set when the fetch/parse failed — the group then keeps
// whatever pattern list it had.
type DynamicGroupDTO struct {
	Key       string `json:"key"`
	Patterns  int    `json:"patterns"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	FetchedAt string `json:"fetchedAt"`
	Error     string `json:"error,omitempty"`
}

// GroupsAddParams creates a custom group. Key is optional — the daemon
// derives a "custom:"-prefixed slug from DisplayName when empty.
type GroupsAddParams struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Patterns    []string `json:"patterns"`
	Color       string   `json:"color"`
}

// GroupsUpdateParams edits an existing custom group, matched by Key.
type GroupsUpdateParams struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Patterns    []string `json:"patterns"`
	Color       string   `json:"color"`
}

// GroupsDeleteParams removes a custom group definition. When DeleteRules is
// true, rules created from the group's patterns are purged too.
type GroupsDeleteParams struct {
	Key         string `json:"key"`
	DeleteRules bool   `json:"deleteRules"`
}

type GroupsIconParams struct {
	Key string `json:"key"`
}

type GroupIconDTO struct {
	Key     string `json:"key"`
	MIME    string `json:"mime"`
	DataB64 string `json:"data"`
}

type GroupsApplyParams struct {
	Key       string `json:"key"`       // group key (e.g. "anthropic")
	Action    string `json:"action"`    // "block" or "route"
	Interface string `json:"interface"` // empty for block, required for route
	Enabled   bool   `json:"enabled"`
}

type GroupsApplyResult struct {
	Created []RuleDTO `json:"created"` // rules that were inserted
	Skipped []string  `json:"skipped"` // patterns skipped because they already exist
}

// GroupsDeleteRulesParams identifies which group's rules to remove.
// The daemon resolves the group key to its pattern list and deletes
// every rule whose pattern matches one of those patterns exactly.
type GroupsDeleteRulesParams struct {
	Key string `json:"key"`
}

// GroupsSetEnabledParams flips the enabled flag on every rule that
// was created from a group's patterns. Same matching rule as delete.
type GroupsSetEnabledParams struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// GroupsBulkResult reports how many rules were touched by a bulk
// operation. RuleIDs is the list affected, in case the UI wants to
// invalidate cached state for them.
type GroupsBulkResult struct {
	Affected int     `json:"affected"`
	RuleIDs  []int64 `json:"ruleIds"`
}

// ProxyDTO is the public view of a stored upstream proxy. Note: the
// Password field is intentionally omitted — list/get/update responses
// never carry it. The UI displays HasPassword instead so the user
// knows credentials are stored without exposing them, and editing
// a proxy without setting Password keeps the previous value.
type ProxyDTO struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"` // "socks5" or "http"
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	HasPassword bool   `json:"hasPassword"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

type ProxiesAddParams struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type ProxiesUpdateParams struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// Password: empty string means "keep existing", any non-empty value
	// replaces the stored password. The UI shows a "leave blank to
	// keep" hint when editing.
	Password string `json:"password"`
}

type ProxiesDeleteParams struct {
	ID int64 `json:"id"`
}

// ProxiesTestParams identifies the proxy to dial. Phase A returns a
// placeholder result; Phase B wires this to an actual connect-and-
// handshake test via core/proxy.Dialer.
type ProxiesTestParams struct {
	ID int64 `json:"id"`
}

type ProxiesTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// XrayDTO is the public view of one stored xray outbound entry.
// Outbound carries the raw JSON the user wrote; the UI's editor reads
// and writes it as a string for round-tripping comments + formatting.
// SocksPort is read-only — it's assigned by the daemon at Add time
// and never changes for the lifetime of the entry.
type XrayDTO struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Outbound  string `json:"outbound"`
	SocksPort int    `json:"socksPort"`
	Enabled   bool   `json:"enabled"`
	// Dialer, when non-empty, marks this entry a "master": its sockopt is
	// tunneled through a leastPing balancer over the referenced nodes.
	// Comma-separated typed refs: xray:NAME / xraysub:NAME / proxy:NAME.
	Dialer    string `json:"dialer"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type XrayAddParams struct {
	Name     string `json:"name"`
	Outbound string `json:"outbound"`
	Enabled  bool   `json:"enabled"`
	Dialer   string `json:"dialer"`
}

type XrayUpdateParams struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Outbound string `json:"outbound"`
	Enabled  bool   `json:"enabled"`
	Dialer   string `json:"dialer"`
}

// XraySubDTO is the public view of a subscription plus its node counts.
type XraySubDTO struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	UserAgent   string `json:"userAgent"`
	IntervalSec int    `json:"intervalSec"`
	NodeCap     int    `json:"nodeCap"`
	Enabled     bool   `json:"enabled"`
	LastFetched string `json:"lastFetched"` // RFC3339, empty if never
	LastError   string `json:"lastError"`
	NodeCount   int    `json:"nodeCount"`
	ActiveCount int    `json:"activeCount"`
	// Data-quota accounting from the provider's Subscription-Userinfo
	// header, byte counters. Expire is a Unix timestamp (0 = never). All
	// zero means the provider never reported quota — frontend hides the bar.
	UsageUpload   int64  `json:"usageUpload"`
	UsageDownload int64  `json:"usageDownload"`
	UsageTotal    int64  `json:"usageTotal"`
	UsageExpire   int64  `json:"usageExpire"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type XraySubAddParams struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	UserAgent   string `json:"userAgent"`
	IntervalSec int    `json:"intervalSec"`
	NodeCap     int    `json:"nodeCap"`
	Enabled     bool   `json:"enabled"`
}

type XraySubUpdateParams struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	UserAgent   string `json:"userAgent"`
	IntervalSec int    `json:"intervalSec"`
	NodeCap     int    `json:"nodeCap"`
	Enabled     bool   `json:"enabled"`
}

type XraySubDeleteParams struct {
	ID int64 `json:"id"`
}

type XraySubSetEnabledParams struct {
	ID      int64 `json:"id"`
	Enabled bool  `json:"enabled"`
}

// XraySubRefreshParams triggers an immediate fetch. ID 0 refreshes every
// subscription that is due.
type XraySubRefreshParams struct {
	ID int64 `json:"id"`
}

type XraySubRefreshResult struct {
	OK      bool   `json:"ok"`
	Nodes   int    `json:"nodes"`
	Message string `json:"message"`
}

type XraySubNodesParams struct {
	SubID int64 `json:"subId"`
}

// XraySubNodeDTO is one node in a subscription's pool. Disabled is the
// durable manual toggle; Active reflects enabled∧¬disabled∧within-cap.
type XraySubNodeDTO struct {
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	Active      bool   `json:"active"`
	Disabled    bool   `json:"disabled"`
	LatencyMs   int    `json:"latencyMs"` // -1 = unknown
}

type XraySubSetNodeDisabledParams struct {
	SubID       int64  `json:"subId"`
	Fingerprint string `json:"fingerprint"`
	Disabled    bool   `json:"disabled"`
}

// XrayObservatoryResult carries the current balancer winners (fingerprints
// of the fastest node each master dialer is routing through), parsed daemon-
// side from `xray api bi` text. Raw is kept for debugging. Empty when no
// dialer slots are running.
type XrayObservatoryResult struct {
	Winners []string `json:"winners"`
	Raw     string   `json:"raw"`
}

type XrayDeleteParams struct {
	ID int64 `json:"id"`
}

type XraySetEnabledParams struct {
	ID      int64 `json:"id"`
	Enabled bool  `json:"enabled"`
}

// XrayStatus is the snapshot the Xray panel polls — surfaces both
// the binary's version (empty if disabled or probe failed) and
// whether the supervisor considers itself enabled (binary on disk).
// PortRangeStart/End are exposed so the UI can warn before the
// allocator fills up.
type XrayStatus struct {
	Enabled        bool     `json:"enabled"`
	Running        bool     `json:"running"`
	Version        string   `json:"version"`
	PortRangeStart int      `json:"portRangeStart"`
	PortRangeEnd   int      `json:"portRangeEnd"`
	LastExit       string   `json:"lastExit"`
	RecentLogs     []string `json:"recentLogs"`
}

// XrayRoutingResult is the round-trip envelope for the global routing
// rules editor — a single string holding a JSON array of rule objects.
// Empty means "no user rules; only the per-entry inbound→outbound
// pairs are emitted".
type XrayRoutingResult struct {
	Rules string `json:"rules"`
}

type XraySetRoutingParams struct {
	Rules string `json:"rules"`
}

// XrayParseLinkParams carries the raw share URI the UI wants converted.
// Supported schemes: vless://, vmess://, trojan://, ss://.
type XrayParseLinkParams struct {
	Link string `json:"link"`
}

// XrayParseLinkResult is what the link parser returns: a suggested
// entry name (derived from the URI fragment, normalized) and the
// outbound JSON block ready to drop into a new entry.
type XrayParseLinkResult struct {
	Name     string `json:"name"`
	Outbound string `json:"outbound"`
}

// XrayTestParams asks the daemon to dial Target through entry ID's
// local SOCKS5 inbound and time the connect+handshake. Target is
// "host:port" (the default in the UI is 1.1.1.1:443). Empty Target
// makes the daemon use 1.1.1.1:443.
type XrayTestParams struct {
	ID     int64  `json:"id"`
	Target string `json:"target"`
}

// XrayTestResult mirrors ProxiesTestResult. OK=true means the
// connect+handshake completed; Message is human-readable (success
// path includes the round-trip duration). ExitIP/Country/Region/City
// come solely from an ip-api.com probe dialed through the entry's own
// SOCKS5 inbound, so they describe the real exit. There is no fallback:
// if the probe fails they are all empty, never the configured server
// address (that is just the first hop, and may resolve to a FakeIP).
type XrayTestResult struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	LatencyMS int    `json:"latencyMs"`
	ExitIP    string `json:"exitIp"`  // actual public exit IP
	Country   string `json:"country"` // ISO 3166-1 alpha-2 code, e.g. "DE"
	Region    string `json:"region"`  // state / province, e.g. "Bavaria"
	City      string `json:"city"`    // city, e.g. "Munich"
}

// ExitIPResult is the public egress identity for one path — the system
// default route (net.public-ip) or a single rule's interface
// (rules.exit-ip). ExitIP/geo come from an ip-api.com probe dialed
// through that path, so they reflect what destination sites actually
// see. Probed=false with a populated Message explains why no IP could
// be determined (block rule, app/proxy not running, probe timeout).
type ExitIPResult struct {
	Interface string `json:"interface"` // "" = system default route; else the rule's interface
	ExitIP    string `json:"exitIp"`    // public egress IP, "" if unknown
	Country   string `json:"country"`   // ISO 3166-1 alpha-2, e.g. "DE"
	Region    string `json:"region"`    // state / province
	City      string `json:"city"`      // city
	Probed    bool   `json:"probed"`    // true if a live exit IP was obtained
	Message   string `json:"message"`   // human-readable note (errors / why no IP)
}

// RuleExitIPParams identifies the rule whose egress to probe. The
// daemon resolves the rule's Interface to a dialer and probes through
// it (block rules short-circuit; allow rules use the default route).
type RuleExitIPParams struct {
	RuleID int64 `json:"ruleId"`
}

// UsageQueryParams selects a window and grouping for proxied-traffic byte
// stats. FromUnix/ToUnix are unix seconds (inclusive). Dimension is one of
// "group", "domain", "proxy"; empty defaults to "domain".
type UsageQueryParams struct {
	FromUnix   int64  `json:"fromUnix"`
	ToUnix     int64  `json:"toUnix"`
	Dimension  string `json:"dimension"`
	BucketSecs int64  `json:"bucketSecs"` // display bucket width; <60 clamps to stored 60s granularity
}

// UsagePointDTO is one aggregated bucket for the dashboard: BucketUnix is
// the 5-minute bucket start (unix seconds), Key is the dimension value
// (group key, domain, or proxy name), and the byte totals are the summed
// sent/received for that bucket+key.
type UsagePointDTO struct {
	BucketUnix int64  `json:"bucketUnix"`
	Key        string `json:"key"`
	BytesSent  int64  `json:"bytesSent"`
	BytesRecv  int64  `json:"bytesRecv"`
}

// ExportSelection picks what goes into an export bundle. When All is true
// the other fields are ignored and everything (all rules + all custom
// groups) is exported. Otherwise the bundle is the union of: the rules in
// RuleIDs, the rules matching each curated group in CuratedKeys, and each
// custom group in CustomKeys (definition + its matching rules).
type ExportSelection struct {
	All         bool     `json:"all"`
	RuleIDs     []int64  `json:"ruleIds"`
	CuratedKeys []string `json:"curatedKeys"`
	CustomKeys  []string `json:"customKeys"`
}

// ExportParams asks the daemon to build and encrypt a bundle. Passphrase is
// mandatory — exports are always encrypted.
type ExportParams struct {
	Selection  ExportSelection `json:"selection"`
	Passphrase string          `json:"passphrase"`
}

// ExportResult carries the encrypted bundle as base64 (the app writes it to
// a file via a save dialog) plus a suggested filename and a content tally.
type ExportResult struct {
	Blob        string `json:"blob"`     // base64 of the encrypted envelope JSON
	Filename    string `json:"filename"` // suggested file name
	RuleCount   int    `json:"ruleCount"`
	GroupCount  int    `json:"groupCount"`
	SubCount    int    `json:"subCount"`
	MasterCount int    `json:"masterCount"`
}

// ImportParams carries a base64-encoded encrypted bundle and its passphrase.
type ImportParams struct {
	Blob       string `json:"blob"`
	Passphrase string `json:"passphrase"`
}

// ImportResult tallies what the import applied. Existing rules (matched by
// pattern) and existing custom groups (matched by key) are skipped, not
// overwritten.
type ImportResult struct {
	RulesCreated   int      `json:"rulesCreated"`
	RulesSkipped   int      `json:"rulesSkipped"`
	GroupsCreated  int      `json:"groupsCreated"`
	GroupsSkipped  int      `json:"groupsSkipped"`
	SubsCreated    int      `json:"subsCreated"`
	SubsSkipped    int      `json:"subsSkipped"`
	MastersCreated int      `json:"mastersCreated"`
	MastersSkipped int      `json:"mastersSkipped"`
	Warnings       []string `json:"warnings"`
}

type SystemDNSStatus struct {
	Active            bool                `json:"active"`            // any service has 127.0.0.1
	Upstream          []string            `json:"upstream"`          // current daemon upstream
	DetectedResolvers []string            `json:"detectedResolvers"` // what scutil sees (excl. loopback)
	PerService        map[string][]string `json:"perService"`        // current per-service DNS
}
