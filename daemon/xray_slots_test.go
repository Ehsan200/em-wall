package main

import (
	"testing"

	"github.com/ehsan/em-wall/core/xray"
)

func slot(idx int, master string, keys ...string) xray.DialerSlot {
	members := make([]xray.DialerMember, len(keys))
	for i, k := range keys {
		members[i] = xray.DialerMember{Key: k}
	}
	return xray.DialerSlot{Index: idx, Master: master, Members: members}
}

func withAliases(s xray.DialerSlot, aliases ...string) xray.DialerSlot {
	s.Aliases = aliases
	return s
}

// sameSlots is what stops Reconcile from restarting xray — and dropping every
// live connection — when nothing has actually changed. It has to be exact:
// live member add/remove moves the running process away from the config on
// disk, and only a member-level comparison notices that.
func TestSameSlots(t *testing.T) {
	base := []xray.DialerSlot{slot(0, "m1", "a", "b"), slot(1, "m2", "c")}

	cases := []struct {
		name string
		b    []xray.DialerSlot
		want bool
	}{
		{"identical", []xray.DialerSlot{slot(0, "m1", "a", "b"), slot(1, "m2", "c")}, true},
		{"member added", []xray.DialerSlot{slot(0, "m1", "a", "b", "z"), slot(1, "m2", "c")}, false},
		{"member changed", []xray.DialerSlot{slot(0, "m1", "a", "z"), slot(1, "m2", "c")}, false},
		{"member reordered", []xray.DialerSlot{slot(0, "m1", "b", "a"), slot(1, "m2", "c")}, false},
		{"slot dropped", []xray.DialerSlot{slot(0, "m1", "a", "b")}, false},
		{"master renamed", []xray.DialerSlot{slot(0, "other", "a", "b"), slot(1, "m2", "c")}, false},
		{"index changed", []xray.DialerSlot{slot(2, "m1", "a", "b"), slot(1, "m2", "c")}, false},
		{"empty vs empty", nil, false},
		{"alias added", []xray.DialerSlot{withAliases(slot(0, "m1", "a", "b"), "m3"), slot(1, "m2", "c")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameSlots(base, tc.b); got != tc.want {
				t.Fatalf("sameSlots = %v, want %v", got, tc.want)
			}
		})
	}
	if !sameSlots(nil, nil) {
		t.Fatalf("sameSlots(nil, nil) = false, want true")
	}
}
