package xray

import (
	"reflect"
	"testing"
)

func TestParseBalancerWinners(t *testing.T) {
	// Exact shape captured from a live `xray api bi slot0-bal` run.
	real := "  - Selecting Override:\n    1                 \n  - Selects:\n    1   slot0-out-fp1 "
	if got := ParseBalancerWinners(real); !reflect.DeepEqual(got, []string{"fp1"}) {
		t.Errorf("real bi output: got %v, want [fp1]", got)
	}

	// Two balancers, deduped across sections, ignoring the Override section.
	multi := `  - Selecting Override:
    1   slot0-out-ignored
  - Selects:
    1   slot0-out-aaa111
  - Selecting Override:
  - Selects:
    1   slot1-out-bbb222
    2   slot0-out-aaa111
`
	got := ParseBalancerWinners(multi)
	if !reflect.DeepEqual(got, []string{"aaa111", "bbb222"}) {
		t.Errorf("multi: got %v, want [aaa111 bbb222]", got)
	}

	// The Override section's tag must NOT be treated as a winner.
	for _, fp := range got {
		if fp == "ignored" {
			t.Errorf("Override-section tag leaked into winners: %v", got)
		}
	}

	if got := ParseBalancerWinners(""); len(got) != 0 {
		t.Errorf("empty input: got %v, want none", got)
	}
	if got := ParseBalancerWinners("garbage without any selects"); len(got) != 0 {
		t.Errorf("no-selects input: got %v, want none", got)
	}
}
