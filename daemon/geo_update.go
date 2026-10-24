package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/geo/meta"
)

// Geo data updater wiring: the daemon side of core/geo's Updater — how to
// reach the network, which tags the running xray config needs, what to do
// once a file changed. See docs/geo-map-suggest.md §1.

// Settings keys.
const (
	settingGeoSource     = "geo_source"      // loyalsoldier (default) | chocolate4u | v2fly | custom
	settingGeoCustomURL  = "geo_custom_url"  // base URL serving geosite.dat + geoip.dat, for custom
	settingGeoAutoUpdate = "geo_auto_update" // "false" turns the hourly due check off; default on
	settingGeoFetchVia   = "geo_fetch_via"   // Interface tried after direct fails; "" = direct only
)

const (
	geoTickInterval   = time.Hour
	geoFirstCheck     = 2 * time.Minute  // after start: let DNS and xray settle first
	geoRunTimeout     = 20 * time.Minute // one whole update run (city mmdb is ~130 MB)
	geoDialTimeout    = 15 * time.Second
	geoHeaderTimeout  = 30 * time.Second
	geoViaCandidates  = 2 // members of a fetch_via binding tried
	geoEnsureInterval = 10 * time.Second
)

// geoState is the per-daemon updater plus the xray follow-up status.
type geoState struct {
	u *geo.Updater
	// xrayNote is set when a .dat changed but xray could not be told
	// live; it then uses the new file from its next (normal) restart.
	xrayNote atomic.Pointer[string]
	// ensuring is set while an on-demand mmdb fetch runs, and lastEnsure
	// rate-limits starting one (map.paths polls every 2 s).
	ensuring   atomic.Bool
	lastEnsure atomic.Int64
}

// geoStates holds one geoState per handlerDeps (in practice one per
// process). Kept out of handlerDeps so the geo feature owns its state.
var geoStates sync.Map // *handlerDeps → *geoState

func (d *handlerDeps) geoUpd() *geoState {
	if v, ok := geoStates.Load(d); ok {
		return v.(*geoState)
	}
	st := &geoState{}
	st.u = &geo.Updater{
		Dir:         filepath.Dir(d.geoDB.Paths().GeoSite),
		DB:          d.geoDB,
		Clients:     d.geoClients,
		Source:      d.geoSource,
		Required:    d.geoRequired,
		OnInstalled: func(ctx context.Context, keys []string) { d.geoInstalled(ctx, st, keys) },
		Logf:        log.Printf,
	}
	v, _ := geoStates.LoadOrStore(d, st)
	return v.(*geoState)
}

// runGeoUpdater reconciles geo.meta with the files on disk, then checks
// for due files shortly after start and hourly after that. Blocks until
// ctx is done; main runs it in its own goroutine.
func (d *handlerDeps) runGeoUpdater(ctx context.Context) {
	if d.geoDB == nil {
		return
	}
	st := d.geoUpd()
	if err := st.u.SyncMeta(); err != nil {
		log.Printf("geo: sync meta: %v", err)
	}
	t := time.NewTimer(geoFirstCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d.geoAutoUpdate(ctx) {
			rctx, cancel := context.WithTimeout(ctx, geoRunTimeout)
			st.u.RunDue(rctx)
			cancel()
		}
		t.Reset(geoTickInterval)
	}
}

// ensureCityData starts fetching the DB-IP City/ASN files when they are
// missing, without blocking: the map handler calls it on every map.paths
// and reads geoDB.Stats().CityLoaded for its "country precision" banner.
// Cheap when there is nothing to do (an atomic and a clock read), and it
// honours geo_auto_update — with auto-update off, the user fetches them
// from Settings → Geo data.
func (d *handlerDeps) ensureCityData() {
	if d.geoDB == nil {
		return
	}
	st := d.geoUpd()
	now := time.Now().UnixNano()
	if last := st.lastEnsure.Load(); now-last < int64(geoEnsureInterval) || !st.lastEnsure.CompareAndSwap(last, now) {
		return
	}
	if st.u.Busy() || !st.ensuring.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer st.ensuring.Store(false)
		ctx, cancel := context.WithTimeout(d.bgContext(), geoRunTimeout)
		defer cancel()
		if !d.geoAutoUpdate(ctx) {
			return
		}
		st.u.EnsureMMDB(ctx)
	}()
}

func (d *handlerDeps) geoAutoUpdate(ctx context.Context) bool {
	v, err := d.store.GetSetting(ctx, settingGeoAutoUpdate, "true")
	return err != nil || v != "false"
}

func (d *handlerDeps) geoSource(ctx context.Context) (geo.Source, error) {
	key, _ := d.store.GetSetting(ctx, settingGeoSource, geo.SourceLoyalsoldier)
	custom, _ := d.store.GetSetting(ctx, settingGeoCustomURL, "")
	return geo.ResolveSource(key, custom)
}

// geoClients is direct first, then — when geo_fetch_via is set — the
// first members of that binding, through the same dialers the exit-IP
// probe uses (an xray entry's SOCKS inbound, a proxy, an interface).
func (d *handlerDeps) geoClients(ctx context.Context) []geo.Client {
	out := []geo.Client{{Name: "direct", HTTP: geoHTTPClient((&net.Dialer{Timeout: geoDialTimeout}).DialContext)}}
	via, _ := d.store.GetSetting(ctx, settingGeoFetchVia, "")
	if via = strings.TrimSpace(via); via == "" {
		return out
	}
	cands, err := d.dialCandidatesForInterface(ctx, via)
	if err != nil {
		log.Printf("geo: fetch via %s: %v", via, err)
		return out
	}
	for i, c := range cands {
		if i >= geoViaCandidates {
			break
		}
		name := "via " + via
		if c.name != "" && c.name != via {
			name += " (" + c.name + ")"
		}
		out = append(out, geo.Client{Name: name, HTTP: geoHTTPClient(c.dc)})
	}
	return out
}

// geoHTTPClient has no overall timeout (a large download over a slow path
// is fine; the run's context bounds it) but bounds each phase before the
// body. It ignores any environment proxy so "direct" is direct.
func geoHTTPClient(dc dialContext) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           dc,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   geoDialTimeout,
		ResponseHeaderTimeout: geoHeaderTimeout,
		DisableKeepAlives:     true,
	}}
}

// geoRequired lists the geosite:/geoip: tags the xray config references:
// the user's routing rules and every enabled entry's outbound JSON.
func (d *handlerDeps) geoRequired(ctx context.Context) (geo.Required, error) {
	if d.xrayStore == nil {
		return geo.Required{}, nil
	}
	routing, err := d.xrayStore.GetRoutingRules(ctx)
	if err != nil {
		return geo.Required{}, err
	}
	entries, err := d.xrayStore.List(ctx)
	if err != nil {
		return geo.Required{}, err
	}
	docs := []string{routing}
	for _, e := range entries {
		if e.Enabled {
			docs = append(docs, e.Outbound)
		}
	}
	return geo.ScanRequired(docs...), nil
}

// geoInstalled runs after files changed on disk. A new .dat reaches xray
// by re-pushing its routing (see geo_xray.go); if that isn't possible the
// file is used from xray's next restart — never a restart just for this.
func (d *handlerDeps) geoInstalled(ctx context.Context, st *geoState, keys []string) {
	dat := false
	for _, k := range keys {
		if k == meta.GeoSite || k == meta.GeoIP {
			dat = true
		}
	}
	if !dat || d.xraySup == nil {
		return
	}
	pushed, err := d.xraySup.repushGeoRouting(ctx)
	switch {
	case err != nil:
		note := "xray keeps the previous geo data until its next restart (live routing refresh failed: " + err.Error() + ")"
		st.xrayNote.Store(&note)
		log.Printf("geo: %s", note)
	case pushed:
		st.xrayNote.Store(nil)
		log.Printf("geo: xray routing re-read the new geo data")
	default:
		st.xrayNote.Store(nil)
	}
}
