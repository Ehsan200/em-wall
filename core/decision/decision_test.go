package decision

import (
	"context"
	"testing"

	"github.com/ehsan/em-wall/core/rules"
)

type staticSource struct{ list []rules.Rule }

func (s staticSource) List(_ context.Context) ([]rules.Rule, error) { return s.list, nil }

func TestEngine_Decide(t *testing.T) {
	src := staticSource{list: []rules.Rule{
		{ID: 1, Pattern: "*.y.com", Action: rules.ActionBlock, Enabled: true},
		{ID: 2, Pattern: "safe.y.com", Action: rules.ActionAllow, Interface: "", Enabled: true},
		{ID: 3, Pattern: "*.work.com", Action: rules.ActionRoute, Interface: "utun3", Enabled: true},
	}}
	e := New(src)
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		wantOutcome Outcome
		wantIface   string
		wantRuleID  int64
	}{
		{"unrelated.com", OutcomeAllow, "", 0},
		{"a.y.com", OutcomeBlock, "", 1},
		{"y.com", OutcomeBlock, "", 1},
		{"safe.y.com", OutcomeAllow, "", 2},
		{"a.work.com", OutcomeRoute, "utun3", 3},
		{"work.com", OutcomeRoute, "utun3", 3},
	}
	for _, tc := range cases {
		got := e.Decide(tc.name)
		if got.Outcome != tc.wantOutcome || got.Interface != tc.wantIface || got.RuleID != tc.wantRuleID {
			t.Errorf("Decide(%q) = %+v, want outcome=%v iface=%q rule=%d",
				tc.name, got, tc.wantOutcome, tc.wantIface, tc.wantRuleID)
		}
	}
}

func TestEngine_EmptyCache(t *testing.T) {
	e := New(staticSource{})
	d := e.Decide("anything.com")
	if d.Outcome != OutcomeAllow {
		t.Errorf("uninitialized engine should default to allow, got %v", d.Outcome)
	}
}

func TestEngine_ReloadPicksUpChanges(t *testing.T) {
	src := &mutableSource{list: []rules.Rule{
		{ID: 1, Pattern: "*.y.com", Action: rules.ActionBlock, Enabled: true},
	}}
	e := New(src)
	_ = e.Reload(context.Background())
	if e.Decide("a.y.com").Outcome != OutcomeBlock {
		t.Fatalf("expected block before change")
	}
	src.list = nil
	_ = e.Reload(context.Background())
	if e.Decide("a.y.com").Outcome != OutcomeAllow {
		t.Errorf("expected allow after rules cleared")
	}
}

type mutableSource struct{ list []rules.Rule }

func (m *mutableSource) List(_ context.Context) ([]rules.Rule, error) { return m.list, nil }
