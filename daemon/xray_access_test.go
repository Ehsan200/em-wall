package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

func TestParseAccessLine(t *testing.T) {
	cases := []struct {
		line    string
		in, out string
		ok      bool
	}{
		// Shapes captured from xray 26.3.27 (see xray_access.go).
		{"2026/10/24 11:45:02.484253 from tcp:127.0.0.1:58821 accepted tcp:127.0.0.1:47199 [slot0-in -> slot0-out-b]", "slot0-in", "slot0-out-b", true},
		{"2026/10/24 11:45:19.297057 from tcp:127.0.0.1:58995 accepted tcp:127.0.0.1:47199 [in-z >> block]", "in-z", "block", true},
		{"2026/10/24 11:45:19.341125 from udp:127.0.0.1:62056 accepted udp:127.0.0.1:47198 [in-r -> out-y]", "in-r", "out-y", true},
		{"2026/10/24 11:45:03.570476 from tcp:127.0.0.1:58860 accepted tcp:[::1]:47199 [in-m -> out-m]", "in-m", "out-m", true},
		{"2026/10/24 11:45:03.570476 from tcp:[::1]:58860 accepted tcp:[2001:db8::1]:443 [slot3-in ==> slot3-out-abc] email: x@y", "slot3-in", "slot3-out-abc", true},
		{"2026/10/24 11:45:19.311579 from tcp:127.0.0.1:58996 rejected  proxy/socks: invalid username or password", "", "", false},
		{"2026/10/24 11:45:19.311579 from tcp:127.0.0.1:58996 accepted tcp:example.com:443 [direct]", "", "", false},
		{"2026/10/24 11:45:19.311579 from tcp:127.0.0.1:58996 accepted tcp:example.com:443", "", "", false},
		{"2026/10/24 11:45:19 [Warning] core: Xray 26.3.27 started", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		in, out, ok := parseAccessLine([]byte(c.line))
		if ok != c.ok || (ok && (string(in) != c.in || string(out) != c.out)) {
			t.Errorf("%q → %q %q %v, want %q %q %v", c.line, in, out, ok, c.in, c.out, c.ok)
		}
	}
}

func accessLine(in, out string) string {
	return "2026/10/24 11:45:02.484253 from tcp:127.0.0.1:1 accepted tcp:srv.example:443 [" + in + " -> " + out + "]"
}

func testSlot() xray.DialerSlot {
	return xray.DialerSlot{Master: "m", Aliases: []string{"m2"}, Index: 0, PoolKey: "xraysub:p", Members: []xray.DialerMember{
		{Key: "fp-a"}, {Key: "fp-b"}, {Key: "fp-c"},
	}}
}

func TestAccessPicksWindow(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	now := base
	a := newXrayAccessLog("")
	a.now = func() time.Time { return now }
	sl := testSlot()
	a2 := xray.SlotMemberTag(0, "fp-a")
	b2 := xray.SlotMemberTag(0, "fp-b")

	// 40 s ago: b picked 5 times. Recently: a 6 times, b once.
	for i := 0; i < 5; i++ {
		a.observe([]byte(accessLine("slot0-in", b2)), base.Add(-40*time.Second))
	}
	for i := 0; i < 6; i++ {
		a.observe([]byte(accessLine("slot0-in", a2)), base.Add(-time.Duration(i)*time.Second))
	}
	a.observe([]byte(accessLine("slot0-in", b2)), base.Add(-10*time.Second))
	// Ignored: the daemon's own probe traffic, the API, an unknown member.
	a.observe([]byte(accessLine("probe-in", a2)), base)
	a.observe([]byte(accessLine("api", "api")), base)
	a.observe([]byte(accessLine("slot0-in", "slot0-out-gone")), base)

	picks, _ := a.slotPicks(sl, accessWindow, 0)
	if len(picks) != 2 || picks[0].tag != a2 || picks[0].recent != 6 || picks[1].picks != 6 || picks[1].recent != 1 {
		t.Fatalf("picks = %+v", picks)
	}
	if !dominant(picks) { // 6 of 7 recent
		t.Error("a should dominate")
	}

	// A minute on, only a's last picks are in the window, and nothing is recent.
	now = base.Add(55 * time.Second)
	picks, _ = a.slotPicks(sl, accessWindow, 0)
	if len(picks) != 1 || picks[0].tag != a2 || picks[0].recent != 0 || dominant(picks) {
		t.Fatalf("later picks = %+v", picks)
	}

	// Another pool on slot index 0 starts from scratch.
	other := sl
	other.PoolKey = "xraysub:q"
	if picks, _ = a.slotPicks(other, accessWindow, 0); len(picks) != 0 {
		t.Fatalf("new pool inherited picks: %+v", picks)
	}
}

func TestAccessHistoryAndPrune(t *testing.T) {
	base := time.Unix(1_800_000_000, 0) // a multiple of 10
	now := base
	a := newXrayAccessLog("")
	a.now = func() time.Time { return now }
	sl := testSlot()
	tag := xray.SlotMemberTag(0, "fp-c")
	a.observe([]byte(accessLine("slot0-in", tag)), base.Add(-25*time.Second)) // bucket -30
	a.observe([]byte(accessLine("slot0-in", tag)), base.Add(-21*time.Second)) // bucket -30
	a.observe([]byte(accessLine("slot0-in", tag)), base.Add(3*time.Second))   // bucket 0
	now = base.Add(5 * time.Second)
	picks, start := a.slotPicks(sl, accessWindow, 4)
	if start != base.Unix()-30 || len(picks) != 1 || !slices.Equal(picks[0].history, []int{2, 0, 0, 1}) {
		t.Fatalf("start %d, picks %+v", start, picks)
	}
	// Entry inbounds keep no history.
	a.observe([]byte(accessLine("in-x", "out-x")), now)
	if c := a.routes["in-x"].outs["out-x"]; c.hist != nil {
		t.Error("entry route allocated history")
	}

	now = base.Add(accessHistSpan + time.Minute)
	a.prune(now)
	if len(a.routes) != 0 {
		t.Errorf("prune left %d routes", len(a.routes))
	}
}

func TestAccessBounded(t *testing.T) {
	a := newXrayAccessLog("")
	now := time.Now()
	for i := 0; i < accessMaxOutbounds+10; i++ {
		a.observe([]byte(accessLine("slot0-in", xray.SlotMemberTag(0, "k"+string(rune('a'+i%26))+string(rune('a'+i/26))))), now)
	}
	for i := 0; i < accessMaxRoutes+10; i++ {
		a.observe([]byte(accessLine("in-e"+string(rune('a'+i%26))+string(rune('a'+i/26)), "out-x")), now)
	}
	if n := len(a.routes["slot0-in"].outs); n != accessMaxOutbounds {
		t.Errorf("outbounds = %d", n)
	}
	if len(a.routes) != accessMaxRoutes {
		t.Errorf("routes = %d", len(a.routes))
	}
	var nilLog *xrayAccessLog
	nilLog.observe([]byte(accessLine("slot0-in", "x")), now)
	if p, _ := nilLog.slotPicks(testSlot(), accessWindow, 0); p != nil {
		t.Error("nil tracker answered")
	}
}

func TestAccessTailFollows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	a := newXrayAccessLog(path)
	tail := newAccessTail(a)

	write := func(s string, flag int) {
		f, err := os.OpenFile(path, flag|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	line := accessLine("in-a", "out-a") + "\n"

	// Lines already there at startup are not replayed.
	write(line+line, os.O_APPEND)
	tail.poll()
	if n := a.lines.Load(); n != 0 || !a.following.Load() {
		t.Fatalf("replayed %d lines at startup (following %v)", n, a.following.Load())
	}
	// Appended lines are read; a partial line waits for its end.
	write(line+line[:30], os.O_APPEND)
	tail.poll()
	if n := a.lines.Load(); n != 1 {
		t.Fatalf("after append: %d lines", n)
	}
	write(line[30:], os.O_APPEND)
	tail.poll()
	if n := a.lines.Load(); n != 2 {
		t.Fatalf("partial line: %d lines", n)
	}
	// Truncated in place (xray's O_APPEND writes land at the new end).
	write("", os.O_TRUNC)
	tail.poll()
	write(line, os.O_APPEND)
	tail.poll()
	if n := a.lines.Load(); n != 3 {
		t.Fatalf("after truncate: %d lines", n)
	}
	// Removed: not following. Recreated: read from its start.
	_ = os.Remove(path)
	tail.poll()
	if following, note := a.status(); following || note == "" {
		t.Fatalf("missing file: following %v note %q", following, note)
	}
	write(line+line, os.O_APPEND)
	tail.poll()
	if n := a.lines.Load(); n != 5 || !a.following.Load() {
		t.Fatalf("recreated: %d lines", n)
	}
}

func TestAccessTailMissingAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	a := newXrayAccessLog(path)
	tail := newAccessTail(a)
	tail.poll()
	// xray starts after the daemon: its first lines are new, not history.
	if err := os.WriteFile(path, []byte(accessLine("in-a", "out-a")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail.poll()
	if n := a.lines.Load(); n != 1 {
		t.Fatalf("lines = %d", n)
	}
}

func TestBuildActivePoolsAndRoutes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := newXrayAccessLog("")
	a.now = func() time.Time { return now }
	sl := testSlot()
	for i := 0; i < 3; i++ {
		a.observe([]byte(accessLine("slot0-in", xray.SlotMemberTag(0, "fp-b"))), now.Add(-time.Duration(i)*time.Second))
	}
	a.observe([]byte(accessLine("slot0-in", xray.SlotMemberTag(0, "fp-a"))), now.Add(-2*time.Second))
	a.observe([]byte(accessLine("in-solo", "out-solo")), now)
	a.observe([]byte(accessLine("in-split", "out-x")), now)
	a.observe([]byte(accessLine("in-split", "direct")), now.Add(-time.Second))
	a.observe([]byte(accessLine("in-split", "direct")), now.Add(-time.Second))

	names := map[string]string{"fp-b": "sub/Berlin"}
	pools := buildActivePools(a, []xray.DialerSlot{sl}, names, accessWindow, 0, now)
	if len(pools) != 1 {
		t.Fatalf("pools = %+v", pools)
	}
	p := pools[0]
	if p.Active != "fp-b" || p.ActiveName != "sub/Berlin" || p.Dominant || p.Picks != 4 ||
		!slices.Equal(p.Masters, []string{"m", "m2"}) || len(p.Members) != 2 {
		t.Fatalf("pool = %+v", p)
	}
	if m := p.Members[0]; m.SharePct != 75 || m.AgoSec != 0 || !m.Active || m.Name != "sub/Berlin" {
		t.Errorf("member = %+v", m)
	}
	if m := p.Members[1]; m.Key != "fp-a" || m.Name != "fp-a" || m.AgoSec != 2 {
		t.Errorf("member = %+v", m)
	}

	routes := buildActiveRoutes(a.inboundPicks(accessWindow), []xray.DialerSlot{sl}, names, now)
	if len(routes) != 2 || routes[0].Entry != "solo" || routes[0].Diverted || routes[1].Entry != "split" ||
		!routes[1].Diverted || routes[1].ActiveName != "direct" || routes[1].Members[1].Name != "x" {
		t.Fatalf("routes = %+v", routes)
	}

	// Through the IPC shape: JSON round-trips with empty slices, not null.
	svc := &xrayActiveService{d: &handlerDeps{}, names: map[string]string{}, namesAt: now.Add(time.Hour)}
	res := svc.active(t.Context(), nil, ipc.XrayActiveParams{})
	raw, _ := json.Marshal(res)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if back["pools"] == nil || back["routes"] == nil || res.Following {
		t.Errorf("nil tracker result = %s", raw)
	}
}
