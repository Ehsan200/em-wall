package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/ehsan/em-wall/core/xray"
)

// The point of live apply, checked against the real xray binary: editing,
// adding and removing entries (and adding a master) must leave a stream
// through an untouched entry alive, and must never restart the process.
//
// Needs an xray binary: EMWALL_XRAY_BIN, else the installed one. Skipped
// in -short mode. The supervisor is built by hand, NOT newXraySupervisor —
// that constructor sweeps "orphan" em-wall-xray processes, which would
// kill the user's live one.
func TestLiveApplyKeepsUntouchedStreams(t *testing.T) {
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
	apiPort, slotPort := freePort(t), freePort(t)
	ports := map[string]int{"a": freePort(t), "b": freePort(t), "c": freePort(t), "m": freePort(t)}
	freedom := func(name string) xray.Config {
		return xray.Config{Name: name, SocksPort: ports[name], Enabled: true, Outbound: `{"protocol":"freedom"}`}
	}
	gen := func(entries []xray.Config, slots []xray.DialerSlot) []byte {
		raw, err := xray.Generate(entries, xray.GenerateOptions{DialerSlots: slots})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// Move the API off the fixed port so a live install is untouched.
		var cfg map[string]any
		_ = json.Unmarshal(raw, &cfg)
		// Same for the slot inbound (and the dialer outbounds aimed at it).
		for _, in := range cfg["inbounds"].([]any) {
			m := in.(map[string]any)
			switch m["tag"] {
			case xray.ApiTag:
				m["port"] = apiPort
			case xray.SlotInboundTag(0):
				m["port"] = slotPort
			}
		}
		for _, ob := range cfg["outbounds"].([]any) {
			m := ob.(map[string]any)
			if tag, _ := m["tag"].(string); strings.HasPrefix(tag, "dialer-") {
				m["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)["port"] = slotPort
			}
		}
		delete(cfg, "log") // keep the test's xray off the real log files
		out, _ := json.Marshal(cfg)
		return out
	}

	dir := t.TempDir()
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

	cfg := gen([]xray.Config{freedom("a"), freedom("b")}, nil)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if err := s.restartLocked(cfgPath); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.running = cfg
	pid := s.cmd.Process.Pid
	s.mu.Unlock()
	waitListening(t, ports["b"])
	waitListening(t, "127.0.0.1:"+strconv.Itoa(apiPort))

	// A long-lived stream through b, which none of the edits touch.
	stream := dialVia(t, ports["b"], echo)
	roundTrip(t, stream, "before")

	apply := func(step string, next []byte) {
		t.Helper()
		s.mu.Lock()
		ok := s.applyLive(context.Background(), next)
		if ok {
			s.running = next
		}
		alive := s.cmd != nil && s.cmd.Process.Pid == pid
		s.mu.Unlock()
		if !ok {
			t.Fatalf("%s: live apply failed; recent xray output:\n%s", step, strings.Join(s.RecentLines(), "\n"))
		}
		if !alive {
			t.Fatalf("%s: xray was restarted", step)
		}
		roundTrip(t, stream, step)
	}

	// Edit a (its outbound changes), add c.
	editedA := freedom("a")
	editedA.Outbound = `{"protocol":"freedom","settings":{"domainStrategy":"UseIP"}}`
	apply("edit a + add c", gen([]xray.Config{editedA, freedom("b"), freedom("c")}, nil))
	waitListening(t, ports["c"])
	roundTrip(t, dialVia(t, ports["c"], echo), "through new c")
	roundTrip(t, dialVia(t, ports["a"], echo), "through edited a")

	// Add a master with a one-member dialer slot.
	m := freedom("m")
	m.Dialer = "xray:c"
	slots := []xray.DialerSlot{{Master: "m", Index: 0, Members: []xray.DialerMember{
		{Key: "xray-c", Outbound: json.RawMessage(`{"protocol":"freedom"}`)},
	}}}
	apply("add master", gen([]xray.Config{editedA, freedom("b"), freedom("c"), m}, slots))
	waitListening(t, ports["m"])
	roundTrip(t, dialVia(t, ports["m"], echo), "through master m via its slot")

	// Remove a.
	apply("remove a", gen([]xray.Config{freedom("b"), freedom("c"), m}, slots))
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(ports["a"]), time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("removed entry a still listening")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func waitListening(t *testing.T, addr any) {
	t.Helper()
	a := ""
	switch v := addr.(type) {
	case int:
		a = "127.0.0.1:" + strconv.Itoa(v)
	case string:
		a = v
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", a, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never started listening", a)
}

func dialVia(t *testing.T, socksPort int, target string) net.Conn {
	t.Helper()
	d, err := xproxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(socksPort), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", target)
	if err != nil {
		t.Fatalf("dial via socks %d: %v", socksPort, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		t.Fatalf("%s: write: %v", msg, err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != msg {
		t.Fatalf("%s: stream broken: %q %v", msg, line, err)
	}
	_ = c.SetDeadline(time.Time{})
}

// The generated metrics endpoint must expose per-node observatory health in
// the shape the parker reads, against the real binary.
func TestObservatoryMetricsReportDeadNode(t *testing.T) {
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
	probe := startHTTP204(t)
	apiPort, slotPort, metricsPort, mPort := freePort(t), freePort(t), freePort(t), freePort(t)
	m := xray.Config{Name: "m", SocksPort: mPort, Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"freedom"}`}
	slots := []xray.DialerSlot{{Master: "m", Index: 0, Members: []xray.DialerMember{
		{Key: "good", Outbound: json.RawMessage(`{"protocol":"freedom"}`)},
		{Key: "dead", Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":` + strconv.Itoa(freePort(t)) + `}]}}`)},
	}}}
	raw, err := xray.Generate([]xray.Config{m}, xray.GenerateOptions{
		DialerSlots: slots, ObservatoryProbeURL: "http://" + probe + "/generate_204", ObservatoryInterval: "1s",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgS := strings.NewReplacer(
		strconv.Itoa(xray.ApiPort), strconv.Itoa(apiPort),
		strconv.Itoa(xray.SlotPort(0)), strconv.Itoa(slotPort),
		strconv.Itoa(xray.MetricsPort), strconv.Itoa(metricsPort),
	).Replace(string(raw))
	var cfg map[string]any
	_ = json.Unmarshal([]byte(cfgS), &cfg)
	delete(cfg, "log")
	out, _ := json.Marshal(cfg)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	_ = os.WriteFile(path, out, 0o644)
	s := &xraySupervisor{
		binaryPath: bin, dataDir: dir, runtimeDir: dir, enabled: true,
		metricsAddr: "127.0.0.1:" + strconv.Itoa(metricsPort),
		logger:      log.New(io.Discard, "", 0), tail: newXrayLineRing(xrayRecentLineCap),
	}
	t.Cleanup(s.Stop)
	s.mu.Lock()
	if err := s.restartLocked(path); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		byTag, err := s.fetchObservatory(context.Background())
		if err == nil {
			good, dead := byTag[xray.SlotMemberTag(0, "good")], byTag[xray.SlotMemberTag(0, "dead")]
			if good.Alive && dead.dead() {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("observatory never reported good alive + dead dead")
}

func startHTTP204(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}
