package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/xray"
)

// After a geo update replaces geosite.dat, a running xray must route by
// the new lists without a restart. Checked against the real binary: a
// rule sends geosite:emtest to the blackhole; v1 of the file doesn't list
// "localhost", v2 does. Swapping the file alone changes nothing (xray only
// reads it while building rules); repushGeoRouting must, with the same
// process.
//
// Needs an xray binary: EMWALL_XRAY_BIN, else the installed one. Skipped
// in -short mode. Built by hand — newXraySupervisor would kill the user's
// live xray (see TestLiveApplyKeepsUntouchedStreams).
func TestGeoRepushRereadsDat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: runs a real xray")
	}
	bin := os.Getenv("EMWALL_XRAY_BIN")
	if bin == "" {
		bin = "/usr/local/bin/em-wall-xray"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no xray binary at %s", bin)
	}

	dir := t.TempDir()
	writeSite := func(names ...string) {
		t.Helper()
		var ds []geo.Domain
		for _, n := range names {
			ds = append(ds, geo.Domain{Type: geo.DomainFull, Value: n})
		}
		b := geo.EncodeGeoSite(map[string][]geo.Domain{"EMTEST": ds})
		// The updater's install is a rename; do the same so the inode changes.
		tmp := filepath.Join(dir, "geosite.dat.tmp")
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "geosite.dat")); err != nil {
			t.Fatal(err)
		}
	}
	writeSite("nothing.invalid")

	echo := startEcho(t)
	_, echoPort, _ := net.SplitHostPort(echo)
	apiPort, metricsPort, socksPort := freePort(t), freePort(t), freePort(t)
	raw, err := xray.Generate(
		[]xray.Config{{Name: "a", SocksPort: socksPort, Enabled: true, Outbound: `{"protocol":"freedom"}`}},
		xray.GenerateOptions{RoutingRules: `[{"type":"field","domain":["geosite:emtest"],"outboundTag":"` + xray.TagBlock + `"}]`},
	)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	for _, in := range cfg["inbounds"].([]any) {
		if m := in.(map[string]any); m["tag"] == xray.ApiTag {
			m["port"] = apiPort
		}
	}
	delete(cfg, "log")
	cfg["metrics"].(map[string]any)["listen"] = "127.0.0.1:" + strconv.Itoa(metricsPort)
	raw, _ = json.Marshal(cfg)

	s := &xraySupervisor{
		binaryPath: bin,
		dataDir:    dir,
		runtimeDir: dir,
		apiAddr:    "127.0.0.1:" + strconv.Itoa(apiPort),
		enabled:    true,
		logger:     log.New(io.Discard, "", 0),
		tail:       newXrayLineRing(xrayRecentLineCap),
	}
	t.Cleanup(s.Stop)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if err := s.restartLocked(cfgPath); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.running = raw
	pid := s.cmd.Process.Pid
	s.mu.Unlock()
	waitListening(t, socksPort)
	waitListening(t, "127.0.0.1:"+strconv.Itoa(apiPort))

	target := "localhost:" + echoPort
	roundTrip(t, dialVia(t, socksPort, target), "v1 passes")

	writeSite("nothing.invalid", "localhost")
	roundTrip(t, dialVia(t, socksPort, target), "file swapped, not re-pushed: still passes")

	pushed, err := s.repushGeoRouting(context.Background())
	if err != nil || !pushed {
		t.Fatalf("repushGeoRouting = %v, %v", pushed, err)
	}
	if !blackholed(dialVia(t, socksPort, target)) {
		t.Fatal("xray still routes by the old geosite.dat after the re-push")
	}
	s.mu.Lock()
	samePid := s.cmd != nil && s.cmd.Process.Pid == pid
	s.mu.Unlock()
	if !samePid {
		t.Fatal("xray was restarted")
	}
}

// blackholed reports whether a stream gets no echo: xray accepts the
// SOCKS CONNECT before routing, so a blackholed stream dials fine and
// then never answers.
func blackholed(c net.Conn) bool {
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		return true
	}
	buf := make([]byte, 5)
	_, err := io.ReadFull(c, buf)
	return err != nil
}
