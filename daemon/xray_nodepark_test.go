package main

import (
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

func parkSlot(keys ...string) []xray.DialerSlot {
	ms := make([]xray.DialerMember, len(keys))
	for i, k := range keys {
		ms[i] = xray.DialerMember{Key: k}
	}
	return []xray.DialerSlot{{Master: "m", Index: 0, Members: ms}}
}

func health(alive map[string]bool) map[string]nodeStatus {
	out := map[string]nodeStatus{}
	for k, a := range alive {
		var st nodeStatus
		st.Alive = a
		st.HealthPing.All = 3
		if !a {
			st.HealthPing.Fail = 3
		}
		out[xray.SlotMemberTag(0, k)] = st
	}
	return out
}

func TestNodeParkerLifecycle(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("good", "dead")
	h := health(map[string]bool{"good": true, "dead": false})

	if ev := p.observe(slots, h); len(ev) != 0 {
		t.Fatalf("parked on first sight: %+v", ev)
	}
	now = now.Add(nodeDeadBeforePark)
	ev := p.observe(slots, h)
	if len(ev) != 1 || ev[0].key != "dead" || !ev[0].parked || ev[0].for_ != nodeParkInitial {
		t.Fatalf("events = %+v, want dead parked for %s", ev, nodeParkInitial)
	}
	if !p.parked()["dead"] {
		t.Fatalf("dead not reported parked")
	}

	// Time up → back on trial.
	now = now.Add(nodeParkInitial)
	if ev := p.release(); len(ev) != 1 || ev[0].parked {
		t.Fatalf("release = %+v, want one trial", ev)
	}
	if p.parked()["dead"] {
		t.Fatalf("still parked after release")
	}
	// Fails through the (short) trial → parked again, twice as long.
	p.observe(slots, h)
	now = now.Add(nodeTrialWindow)
	ev = p.observe(slots, h)
	if len(ev) != 1 || ev[0].for_ != 2*nodeParkInitial {
		t.Fatalf("re-park = %+v, want %s", ev, 2*nodeParkInitial)
	}

	// Recovers on a later trial → history forgotten.
	now = now.Add(2 * nodeParkInitial)
	p.release()
	p.observe(slots, health(map[string]bool{"good": true, "dead": true}))
	if len(p.nodes) != 0 {
		t.Fatalf("recovered node kept history: %+v", p.nodes)
	}
}

// Everything dead at once is an uplink outage: nothing may be parked, or
// the pool would stay empty after the link comes back.
func TestNodeParkerNeverParksWithoutLiveSibling(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("a", "b")
	h := health(map[string]bool{"a": false, "b": false})
	p.observe(slots, h)
	now = now.Add(time.Hour)
	if ev := p.observe(slots, h); len(ev) != 0 {
		t.Fatalf("parked during total outage: %+v", ev)
	}
}

func TestNodeParkerCapsBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("good", "dead")
	h := health(map[string]bool{"good": true, "dead": false})
	p.observe(slots, h)
	now = now.Add(nodeDeadBeforePark)
	p.observe(slots, h)
	var last time.Duration
	for i := 0; i < 10; i++ {
		now = now.Add(nodeParkMax)
		p.release()
		p.observe(slots, h)
		now = now.Add(nodeTrialWindow)
		if ev := p.observe(slots, h); len(ev) == 1 {
			last = ev[0].for_
		}
	}
	if last != nodeParkMax {
		t.Fatalf("park length = %s, want capped at %s", last, nodeParkMax)
	}
}

func TestWithoutParkedKeepsLastMember(t *testing.T) {
	ms := []xray.DialerMember{{Key: "a"}, {Key: "b"}}
	if got := withoutParked(ms, map[string]bool{"a": true}); len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("got %+v", got)
	}
	if got := withoutParked(ms, map[string]bool{"a": true, "b": true}); len(got) != 2 {
		t.Fatalf("emptied the pool: %+v", got)
	}
}

func TestParseObservatoryVars(t *testing.T) {
	body := []byte(`{"cmdline":[],"observatory":{"slot0-out-dead":{"outbound_tag":"slot0-out-dead","health_ping":{"all":3,"fail":3}},"slot0-out-good":{"alive":true,"delay":517,"health_ping":{"all":3}}}}`)
	m, err := parseObservatoryVars(body)
	if err != nil {
		t.Fatal(err)
	}
	if !m["slot0-out-dead"].dead() || m["slot0-out-good"].dead() || !m["slot0-out-good"].Alive {
		t.Fatalf("parsed = %+v", m)
	}
}
