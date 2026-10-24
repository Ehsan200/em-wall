package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

func geoTestDeps(t *testing.T) *handlerDeps {
	t.Helper()
	dir := t.TempDir()
	store, err := rules.Open(filepath.Join(dir, "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	xs, err := xray.Open(filepath.Join(dir, "xray.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xs.Close() })
	ps, err := proxy.Open(filepath.Join(dir, "proxies.db"))
	if err != nil {
		t.Fatal(err)
	}
	site := geo.EncodeGeoSite(map[string][]geo.Domain{
		"SPOTIFY": {{Type: geo.DomainDomain, Value: "spotify.com"}},
	})
	if err := os.WriteFile(filepath.Join(dir, "geosite.dat"), site, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &handlerDeps{store: store, xrayStore: xs, proxyStore: ps, geoDB: geo.Open(geo.Paths{
		GeoSite:  filepath.Join(dir, "geosite.dat"),
		GeoIP:    filepath.Join(dir, "geoip.dat"),
		CityMMDB: filepath.Join(dir, "dbip-city-lite.mmdb"),
		ASNMMDB:  filepath.Join(dir, "dbip-asn-lite.mmdb"),
	})}
	t.Cleanup(func() { geoStates.Delete(d) })
	return d
}

func TestGeoRequiredScansRoutingAndEntries(t *testing.T) {
	d := geoTestDeps(t)
	ctx := context.Background()
	if err := d.xrayStore.SetRoutingRules(ctx, `[{"type":"field","domain":["geosite:spotify"],"outboundTag":"direct"},{"type":"field","ip":["geoip:telegram"],"outboundTag":"block"}]`); err != nil {
		t.Fatal(err)
	}
	req, err := d.geoRequired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (geo.Required{Site: []string{"SPOTIFY"}, IP: []string{"TELEGRAM"}}); !reflect.DeepEqual(req, want) {
		t.Errorf("required = %+v, want %+v", req, want)
	}
}

func TestGeoStatusReportsFilesAndSettings(t *testing.T) {
	d := geoTestDeps(t)
	ctx := context.Background()
	if err := d.geoUpd().u.SyncMeta(); err != nil {
		t.Fatal(err)
	}
	_ = d.store.SetSetting(ctx, settingGeoAutoUpdate, "false")
	_ = d.store.SetSetting(ctx, settingGeoFetchVia, "xrayset:nj")

	st := d.geoStatus(ctx)
	if st.GeositeSource != geo.SourceLoyalsoldier || st.AutoUpdate || st.FetchVia != "xrayset:nj" || st.Updating {
		t.Errorf("settings = %+v", st)
	}
	if len(st.Files) != 4 {
		t.Fatalf("files = %+v", st.Files)
	}
	gs := st.Files[0]
	if gs.File != "geosite" || !gs.Present || gs.Source != geo.SourceBundled || gs.Entries != 1 || gs.SizeBytes == 0 || gs.NextDue != "" {
		t.Errorf("geosite = %+v (auto-update off: no NextDue)", gs)
	}
	if city := st.Files[2]; city.File != "city" || city.Present {
		t.Errorf("city = %+v", city)
	}
}

func TestGeoClientsDirectFirst(t *testing.T) {
	d := geoTestDeps(t)
	ctx := context.Background()
	if cs := d.geoClients(ctx); len(cs) != 1 || cs[0].Name != "direct" {
		t.Errorf("no fetch_via: %+v", cs)
	}
	// An unresolvable binding leaves direct as the only way, not an error.
	_ = d.store.SetSetting(ctx, settingGeoFetchVia, "proxy:nope")
	if cs := d.geoClients(ctx); len(cs) != 1 {
		t.Errorf("bad fetch_via: %+v", cs)
	}
}
