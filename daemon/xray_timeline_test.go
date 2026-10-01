package main

import (
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// timelineRig drives a poolTimeline over one slot of members a..e with
// a and b shortlisted (a = fallback).
type timelineRig struct {
	t    *testing.T
	tl   *poolTimeline
	at   time.Time
	slot xray.DialerSlot
}

func newTimelineRig(t *testing.T) *timelineRig {
	r := &timelineRig{t: t, tl: newPoolTimeline(), at: time.Unix(1_000_000, 0)}
	r.tl.now = func() time.Time { return r.at }
	r.slot = xray.DialerSlot{Master: "m", Aliases: []string{"m2"}, Index: 0, Preferred: []string{"a", "b"}}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		r.slot.Members = append(r.slot.Members, xray.DialerMember{Key: k})
	}
	return r
}

// round records one sample. up lists members answering every ping; the
// rest of the slot's members fail every ping.
func (r *timelineRig) round(up string, traffic map[string]byteCount, parked map[string]bool) {
	r.t.Helper()
	byTag := map[string]nodeStatus{}
	for _, m := range r.slot.Members {
		st := ping(100, 5, 6, 6)
		for _, u := range up {
			if string(u) == m.Key {
				st = ping(100, 5, 6, 0)
			}
		}
		byTag[xray.SlotMemberTag(r.slot.Index, m.Key)] = st
	}
	tagged := map[string]byteCount{}
	for k, c := range traffic {
		tagged[xray.SlotMemberTag(r.slot.Index, k)] = c
	}
	r.tl.record([]xray.DialerSlot{r.slot}, byTag, tagged, parked)
	r.at = r.at.Add(nodeTimelineInterval)
}

func (r *timelineRig) pool() ipc.PoolTimelineDTO {
	r.t.Helper()
	got := r.tl.snapshot("", 0, map[string]string{"a": "sub/alpha"})
	if len(got) != 1 {
		r.t.Fatalf("pools = %d, want 1", len(got))
	}
	return got[0]
}

func node(p ipc.PoolTimelineDTO, key string) ipc.PoolNodeTimelineDTO {
	for _, n := range p.Nodes {
		if n.Key == key {
			return n
		}
	}
	return ipc.PoolNodeTimelineDTO{}
}

func TestTimelineWavesAndPickLoss(t *testing.T) {
	r := newTimelineRig(t)
	// Wave 1: a,b (the shortlist) up. Wave 2: they die, c,d answer — a
	// pick loss. Uplink outage: nobody answers — not a pick loss, and not
	// a flip. Wave 1 again, then wave 2 again: the second pick loss.
	for _, up := range []string{"ab", "ab", "cd", "cd", "", "", "ab", "cd"} {
		r.round(up, nil, nil)
	}
	p := r.pool()
	if p.PickLosses != 2 {
		t.Errorf("pick losses = %d, want 2", p.PickLosses)
	}
	if want := []bool{false, false, false, false, true, true, false, false}; len(p.UplinkDown) != len(want) {
		t.Fatalf("uplinkDown len %d", len(p.UplinkDown))
	} else {
		for i := range want {
			if p.UplinkDown[i] != want[i] {
				t.Errorf("uplinkDown[%d] = %v, want %v", i, p.UplinkDown[i], want[i])
			}
		}
	}
	a := node(p, "a")
	if a.States != "AADDDDAD" {
		t.Errorf("a states = %q", a.States)
	}
	if a.Roles != "ffffffff" || a.Role != "f" {
		t.Errorf("a roles = %q role %q, want fallback throughout", a.Roles, a.Role)
	}
	// up,up,down,down,[uplink skipped],up,down → 3 flips; 3 of 6 observed up.
	if a.Flips != 3 || a.UptimePct != 50 {
		t.Errorf("a flips %d uptime %.0f, want 3 / 50", a.Flips, a.UptimePct)
	}
	if a.Name != "sub/alpha" || node(p, "c").Name != "c" {
		t.Errorf("names: a=%q c=%q", a.Name, node(p, "c").Name)
	}
	if e := node(p, "e"); e.Flips != 0 || e.UptimePct != 0 {
		t.Errorf("e (always dead) flips %d uptime %.0f", e.Flips, e.UptimePct)
	}
	if p.Flappers != 4 || p.ChurnPct != 80 {
		t.Errorf("flappers %d churn %.0f, want 4 / 80", p.Flappers, p.ChurnPct)
	}
	// Carriers first: fallback, active, then the idle ones.
	if p.Nodes[0].Key != "a" || p.Nodes[1].Key != "b" {
		t.Errorf("order = %s,%s…", p.Nodes[0].Key, p.Nodes[1].Key)
	}
	if len(p.Masters) != 2 || p.Masters[0] != "m" {
		t.Errorf("masters = %v", p.Masters)
	}
}

func TestTimelineTrafficDeltas(t *testing.T) {
	r := newTimelineRig(t)
	r.round("ab", map[string]byteCount{"a": {up: 1000, down: 5000}}, nil)
	r.round("ab", map[string]byteCount{"a": {up: 1500, down: 9000}}, nil)
	r.round("ab", map[string]byteCount{"a": {up: 200, down: 100}}, nil) // xray restarted
	a := node(r.pool(), "a")
	// First reading has nothing to diff against.
	if a.UpBytes[0] != 0 || a.UpBytes[1] != 500 || a.DownBytes[1] != 4000 || a.UpBytes[2] != 200 {
		t.Errorf("up %v down %v", a.UpBytes, a.DownBytes)
	}
	if a.TotalUp != 700 || a.TotalDown != 4100 {
		t.Errorf("totals %d / %d", a.TotalUp, a.TotalDown)
	}
}

func TestTimelineParkedCarriedForward(t *testing.T) {
	r := newTimelineRig(t)
	r.round("ab", nil, nil)
	// e is parked: gone from Members, still on its strip as P.
	r.slot.Members = r.slot.Members[:4]
	r.round("ab", nil, map[string]bool{"e": true})
	r.round("ab", nil, map[string]bool{"e": true})
	// Released and back: a member again.
	r.slot.Members = append(r.slot.Members, xray.DialerMember{Key: "e"})
	r.round("abe", nil, nil)
	if e := node(r.pool(), "e"); e.States != "DPPA" || e.Roles != "i  i" {
		t.Errorf("e states %q roles %q", e.States, e.Roles)
	}
}

func TestTimelineRingAndWindow(t *testing.T) {
	r := newTimelineRig(t)
	for i := 0; i < nodeTimelineCap+5; i++ {
		r.round("ab", nil, nil)
	}
	if p := r.pool(); len(p.Times) != nodeTimelineCap {
		t.Errorf("kept %d samples, want %d", len(p.Times), nodeTimelineCap)
	}
	got := r.tl.snapshot("m2", time.Minute, nil)
	if len(got) != 1 || len(got[0].Times) != 6 {
		t.Errorf("1m window via alias: %d pools / %v", len(got), got)
	}
	if got := r.tl.snapshot("other", 0, nil); len(got) != 0 {
		t.Errorf("unknown master matched %d pools", len(got))
	}
	// The slot is unloaded: its history goes with it.
	r.tl.record(nil, nil, nil, nil)
	if got := r.tl.snapshot("", 0, nil); len(got) != 0 {
		t.Errorf("unloaded pool kept: %d", len(got))
	}
}

func TestSlotRoles(t *testing.T) {
	members := func(keys ...string) []xray.DialerMember {
		var out []xray.DialerMember
		for _, k := range keys {
			out = append(out, xray.DialerMember{Key: k})
		}
		return out
	}
	got := slotRoles(xray.DialerSlot{Members: members("a", "b", "c"), Preferred: []string{"gone", "c"}})
	if got["c"] != ipc.PoolRoleFallback || got["a"] != ipc.PoolRoleIdle {
		t.Errorf("shortlist roles = %q", got)
	}
	got = slotRoles(xray.DialerSlot{Members: members("a", "b")})
	if got["a"] != ipc.PoolRoleFallback || got["b"] != ipc.PoolRoleActive {
		t.Errorf("small slot roles = %q", got)
	}
	got = slotRoles(xray.DialerSlot{Members: members("a", "b", "c")})
	if got["a"] != ipc.PoolRoleFallback || got["b"] != ipc.PoolRoleUnranked {
		t.Errorf("unranked roles = %q", got)
	}
}

func TestParseMetricsVarsStats(t *testing.T) {
	body := []byte(`{"observatory":{"slot0-out-a":{"alive":true,"delay":90,"health_ping":{"all":6,"fail":1}}},
	  "stats":{"inbound":{},"outbound":{"slot0-out-a":{"downlink":1051,"uplink":95}},"user":{}}}`)
	m, err := parseMetricsVars(body)
	if err != nil {
		t.Fatal(err)
	}
	if c := m.outbound["slot0-out-a"]; c.up != 95 || c.down != 1051 {
		t.Errorf("outbound = %+v", m.outbound)
	}
	if st := m.observatory["slot0-out-a"]; cellState(st) != ipc.PoolCellFlaky {
		t.Errorf("state = %c", cellState(st))
	}
}
