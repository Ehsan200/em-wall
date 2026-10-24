package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/ehsan/em-wall/core/xray"
)

// The access log must name the member a slot balancer picked — not the
// balancer — on the slot inbound's line, and the tracker must count it,
// against the real binary and the generated config. Path-probe traffic
// through the probe inbound and observatory pings must not count.
//
// Same rules as TestLiveApplyKeepsUntouchedStreams: EMWALL_XRAY_BIN or the
// installed binary, skipped in -short, supervisor built by hand. Every
// fixed port (API, metrics, slot, probe) is moved to a free one so the
// installed daemon's xray is untouched, and the log goes to a temp dir.
func TestAccessLogNamesBalancerPick(t *testing.T) {
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

	echo := startEcho(t)
	ping := startHTTP204(t)
	apiPort, slotPort, probePort, metricsPort, mPort := freePort(t), freePort(t), freePort(t), freePort(t), freePort(t)
	logDir := t.TempDir()

	entries := []xray.Config{{Name: "m", SocksPort: mPort, Enabled: true, Dialer: "xraysub:p", Outbound: `{"protocol":"freedom"}`}}
	slot := xray.DialerSlot{Master: "m", Index: 0, PoolKey: "xraysub:p", Members: []xray.DialerMember{
		{Key: "fp-a", Outbound: json.RawMessage(`{"protocol":"freedom"}`)},
		{Key: "fp-b", Outbound: json.RawMessage(`{"protocol":"freedom"}`)},
	}}
	raw, err := xray.Generate(entries, xray.GenerateOptions{
		LogDir: logDir, DialerSlots: []xray.DialerSlot{slot},
		ObservatoryProbeURL: "http://" + ping + "/generate_204", ObservatoryInterval: "1s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	for _, in := range cfg["inbounds"].([]any) {
		m := in.(map[string]any)
		switch m["tag"] {
		case xray.ApiTag:
			m["port"] = apiPort
		case xray.SlotInboundTag(0):
			m["port"] = slotPort
		case xray.ProbeTag:
			m["port"] = probePort
		}
	}
	for _, ob := range cfg["outbounds"].([]any) {
		m := ob.(map[string]any)
		if m["tag"] == xray.DialerOutboundTag("m") {
			m["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)["port"] = slotPort
		}
	}
	cfg["metrics"].(map[string]any)["listen"] = "127.0.0.1:" + strconv.Itoa(metricsPort)
	out, _ := json.Marshal(cfg)

	dir := t.TempDir()
	s := &xraySupervisor{
		binaryPath: bin, dataDir: dir, runtimeDir: dir, enabled: true,
		apiAddr: "127.0.0.1:" + strconv.Itoa(apiPort),
		logger:  log.New(io.Discard, "", 0), tail: newXrayLineRing(xrayRecentLineCap),
	}
	t.Cleanup(s.Stop)
	cfgPath := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfgPath, out, 0o644)
	s.mu.Lock()
	if err := s.restartLocked(cfgPath); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	waitListening(t, mPort)
	waitListening(t, probePort)

	logPath := filepath.Join(logDir, xrayAccessLogName)
	a := newXrayAccessLog(logPath)
	tail := newAccessTail(a)
	tail.started = true // the file is xray's from its first line: read it all

	time.Sleep(2500 * time.Millisecond) // a few observatory rounds
	const conns = 6
	for i := 0; i < conns; i++ {
		roundTrip(t, dialVia(t, mPort, echo), "via master "+strconv.Itoa(i))
	}
	// One path-probe connection: the daemon's own, never a pick.
	pd, err := xproxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(probePort),
		&xproxy.Auth{User: xray.SlotMemberTag(0, "fp-a"), Password: xray.ProbePassword}, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := pd.Dial("tcp", echo); err == nil {
		_ = c.Close()
	}

	// xray buffers its access log writes briefly; poll until they land.
	deadline := time.Now().Add(5 * time.Second)
	var picks []outPick
	for time.Now().Before(deadline) {
		tail.poll()
		picks, _ = a.slotPicks(slot, accessWindow, 0)
		total := 0
		for _, p := range picks {
			total += p.picks
		}
		if total >= conns {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	body, _ := os.ReadFile(logPath)
	t.Logf("access log:\n%s", body)
	total := 0
	for _, p := range picks {
		if _, ok := xray.SlotMemberKey(p.tag); !ok {
			t.Errorf("pick on %q, not a slot member", p.tag)
		}
		total += p.picks
	}
	if total != conns {
		t.Fatalf("counted %d slot picks, want %d (one per master connection): %+v", total, conns, picks)
	}
	for _, ln := range strings.Split(string(body), "\n") {
		if strings.Contains(ln, xray.SlotBalancerTag(0)) {
			t.Errorf("a line names the balancer, not its pick: %q", ln)
		}
	}
	if r := a.inboundPicks(accessWindow)["in-m"]; len(r) != 1 || r[0].tag != "out-m" || r[0].picks != conns {
		t.Errorf("master inbound picks = %+v", r)
	}
}
