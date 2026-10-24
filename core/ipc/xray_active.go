package ipc

// xray.active: which pool node / outbound xray is picking right now, read
// from xray's access log (one line per routing decision). Lives in its own
// file so it doesn't collide with edits to protocol.go.

// MethodXrayActive returns XrayActiveResult. Params: XrayActiveParams
// (optional).
const MethodXrayActive = "xray.active"

// XrayActiveParams tunes the reply. WindowSec ≤ 0 = the default (60 s,
// which is also the cap: the daemon keeps no finer history than that).
// History adds each pool member's per-10s pick counts over the last 30 min
// (the pool timeline's span) — off by default, the Xray tab doesn't need it.
type XrayActiveParams struct {
	WindowSec int  `json:"windowSec,omitempty"`
	History   bool `json:"history,omitempty"`
}

// XrayActiveResult is one snapshot. Following is false when the daemon
// isn't reading the access log (xray disabled, logging off, file missing);
// Note says why. Pools and Routes are then empty.
type XrayActiveResult struct {
	Following   bool                 `json:"following"`
	Note        string               `json:"note,omitempty"`
	WindowSec   int                  `json:"windowSec"`
	RecentSec   int                  `json:"recentSec"`   // the "now" sub-window behind Recent / Active / Dominant
	GeneratedAt int64                `json:"generatedAt"` // unix ms
	Pools       []XrayActivePoolDTO  `json:"pools"`
	Routes      []XrayActiveRouteDTO `json:"routes"`
}

// XrayActivePoolDTO is one master pool (dialer slot): which members the
// slot balancer handed connections to. Master is the slot's owner, Masters
// every master sharing it (owner first). Active is the member with the most
// picks in the recent sub-window ("" = no picks at all); Dominant means it
// took nearly all of them, so it is carrying the pool's new connections.
// Older long-lived connections may still ride other members — the log only
// sees connections being opened.
type XrayActivePoolDTO struct {
	Master     string                `json:"master"`
	Masters    []string              `json:"masters"`
	Slot       int                   `json:"slot"`
	Active     string                `json:"active"`     // member key
	ActiveName string                `json:"activeName"` // its display name
	Dominant   bool                  `json:"dominant"`
	Picks      int                   `json:"picks"`  // in the window
	Recent     int                   `json:"recent"` // in the recent sub-window
	Members    []XrayActiveMemberDTO `json:"members"`
	// History layout (only with XrayActiveParams.History): bucket i of a
	// member's History covers [HistoryStart + i·BucketSec, +BucketSec).
	BucketSec    int   `json:"bucketSec,omitempty"`
	HistoryStart int64 `json:"historyStart,omitempty"` // unix seconds
}

// XrayActiveMemberDTO is one outbound picked at least once in the window
// (pool members with no picks are left out). For a pool, Key is the member
// key (subscription node fingerprint, "xray-NAME", "proxy-NAME"); for a
// route, the outbound tag.
type XrayActiveMemberDTO struct {
	Key        string  `json:"key"`
	Name       string  `json:"name"`
	Picks      int     `json:"picks"`
	Recent     int     `json:"recent"`
	SharePct   float64 `json:"sharePct"`   // of the window's picks
	LastPicked int64   `json:"lastPicked"` // unix ms
	AgoSec     float64 `json:"agoSec"`     // since LastPicked, at GeneratedAt
	Active     bool    `json:"active"`     // picked in the recent sub-window
	History    []int   `json:"history,omitempty"`
}

// XrayActiveRouteDTO is one entry's inbound and the outbounds its traffic
// was routed to. Normally that is just the entry's own outbound; Diverted
// is set when user routing rules (or a balancer they name) sent it
// anywhere else — that's the case worth showing. Active is the busiest
// outbound tag in the recent sub-window, ActiveName its display form
// (entry name, "direct", "block", a pool node's name).
type XrayActiveRouteDTO struct {
	Entry      string                `json:"entry"`
	Inbound    string                `json:"inbound"`
	Active     string                `json:"active"`
	ActiveName string                `json:"activeName"`
	Diverted   bool                  `json:"diverted"`
	Picks      int                   `json:"picks"`
	Recent     int                   `json:"recent"`
	Members    []XrayActiveMemberDTO `json:"members"`
}
