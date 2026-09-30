package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

func ping(avgMs, devMs int64, all, fail int) nodeStatus {
	var st nodeStatus
	st.Alive = fail < all
	st.Delay = avgMs
	st.HealthPing.All = all
	st.HealthPing.Fail = fail
	st.HealthPing.Average = avgMs * int64(time.Millisecond)
	st.HealthPing.Deviation = devMs * int64(time.Millisecond)
	return st
}

func TestNodeScore(t *testing.T) {
	if _, ok := nodeScore(ping(100, 10, 2, 0)); ok {
		t.Error("scored a node with too few pings")
	}
	if _, ok := nodeScore(ping(100, 10, 6, 4)); ok {
		t.Error("scored a node failing beyond the balancer tolerance")
	}
	if _, ok := nodeScore(ping(0, 0, 6, 6)); ok {
		t.Error("scored a dead node")
	}
	// The fast, slightly jittery node must beat the slow, steady one —
	// the case leastLoad's deviation-first sort gets backwards.
	fast, _ := nodeScore(ping(120, 20, 6, 0))
	steady, _ := nodeScore(ping(700, 1, 6, 0))
	if fast >= steady {
		t.Errorf("120±20ms scored %v, 700±1ms scored %v: want the fast node cheaper", fast, steady)
	}
	// A lost ping costs: same RTT, one failure in six.
	clean, _ := nodeScore(ping(200, 10, 6, 0))
	lossy, _ := nodeScore(ping(200, 10, 6, 1))
	if lossy <= clean {
		t.Errorf("lossy %v <= clean %v", lossy, clean)
	}
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPickShortlist(t *testing.T) {
	scores := map[string]time.Duration{"a": ms(300), "b": ms(100), "c": ms(200), "d": ms(900)}

	// From nothing: the best two, best first.
	if got := pickShortlist(nil, scores, 2); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("fresh = %v, want [b c]", got)
	}
	// Nothing qualified (uplink down): keep what we have.
	if got := pickShortlist([]string{"a", "b"}, nil, 2); got != nil {
		t.Errorf("empty scores = %v, want nil (keep)", got)
	}
	// A near-equal challenger does not displace an incumbent…
	near := map[string]time.Duration{"a": ms(200), "b": ms(100), "c": ms(180)}
	if got := pickShortlist([]string{"b", "a"}, near, 2); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Errorf("near-equal = %v, want [b a] unchanged", got)
	}
	// …a clearly better one replaces the weakest.
	if got := pickShortlist([]string{"b", "a"}, scores, 2); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("clear winner = %v, want [b c]", got)
	}
	// An incumbent that stops qualifying loses its seat at once, even to a
	// worse member.
	gone := map[string]time.Duration{"b": ms(100), "d": ms(900)}
	if got := pickShortlist([]string{"b", "a"}, gone, 2); !reflect.DeepEqual(got, []string{"b", "d"}) {
		t.Errorf("disqualified incumbent = %v, want [b d]", got)
	}
	// The head (the balancer fallback) moves only for a clear improvement.
	tie := map[string]time.Duration{"a": ms(110), "b": ms(100)}
	if got := pickShortlist([]string{"a", "b"}, tie, 2); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("near-tie head = %v, want [a b] unchanged", got)
	}
	clear := map[string]time.Duration{"a": ms(400), "b": ms(100)}
	if got := pickShortlist([]string{"a", "b"}, clear, 2); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Errorf("clear head = %v, want [b a]", got)
	}
}

func TestSlotShortlistObserve(t *testing.T) {
	ob := []byte(`{"protocol":"freedom"}`)
	slot := xray.DialerSlot{Master: "m", Index: 0}
	for _, k := range []string{"a", "b", "c"} {
		slot.Members = append(slot.Members, xray.DialerMember{Key: k, Outbound: ob})
	}
	byTag := map[string]nodeStatus{
		xray.SlotMemberTag(0, "a"): ping(700, 1, 6, 0),
		xray.SlotMemberTag(0, "b"): ping(120, 20, 6, 0),
		xray.SlotMemberTag(0, "c"): ping(200, 30, 6, 0),
	}
	sl := newSlotShortlist()
	ch := sl.observe([]xray.DialerSlot{slot}, byTag)
	if len(ch) != 1 || !reflect.DeepEqual(ch[0].to, []string{"b", "c"}) {
		t.Fatalf("changes = %+v, want one slot → [b c]", ch)
	}
	if got := sl.preferred("m", slot.Members); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("preferred = %v", got)
	}
	// Same data again: nothing to apply.
	if ch := sl.observe([]xray.DialerSlot{slot}, byTag); len(ch) != 0 {
		t.Errorf("repeat observe changed %+v", ch)
	}
	// A slot no bigger than the balancer's spread gets no shortlist.
	small := slot
	small.Members = slot.Members[:2]
	if got := sl.preferred("m", small.Members); got != nil {
		t.Errorf("small slot preferred = %v, want nil", got)
	}
	// A master that no longer has a slot is forgotten.
	sl.observe(nil, byTag)
	if got := sl.preferred("m", slot.Members); len(got) != 0 {
		t.Errorf("stale shortlist kept: %v", got)
	}
	// nil receiver is inert.
	var nilSL *slotShortlist
	if nilSL.observe([]xray.DialerSlot{slot}, byTag) != nil || nilSL.preferred("m", slot.Members) != nil {
		t.Error("nil shortlist did something")
	}
}
