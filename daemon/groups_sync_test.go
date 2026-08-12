package main

import (
	"testing"

	"github.com/ehsan/em-wall/core/rules"
)

func rulesFrom(shape func(int) rules.Rule, patterns ...string) []rules.Rule {
	out := make([]rules.Rule, 0, len(patterns))
	for i, p := range patterns {
		r := shape(i)
		r.Pattern = p
		out = append(out, r)
	}
	return out
}

func routed(iface string) func(int) rules.Rule {
	return func(int) rules.Rule {
		return rules.Rule{Action: rules.ActionRoute, Interface: iface, Enabled: true}
	}
}

// The core case: an app update adds a pattern to a group the user already
// applied. Only the added pattern is missing.
func TestMissingGroupPatterns_ReportsOnlyTheNewOnes(t *testing.T) {
	group := []string{"*.airbnb.com", "*.muscache.com", "*.airbnb.net"}
	stored := rulesFrom(routed("proxy:main"), "*.airbnb.com", "*.muscache.com")

	got := missingGroupPatterns(group, stored)
	if len(got) != 1 || got[0] != "*.airbnb.net" {
		t.Fatalf("missing = %v, want [*.airbnb.net]", got)
	}
}

// A group nobody applied is not drift — otherwise every group in the
// registry would light up the sync banner on a fresh install.
func TestMissingGroupPatterns_UnappliedGroupIsNotDrift(t *testing.T) {
	group := []string{"*.pandora.com", "*.p-cdn.com"}
	stored := rulesFrom(routed("proxy:main"), "*.openai.com")

	if got := missingGroupPatterns(group, stored); got != nil {
		t.Fatalf("missing = %v, want nil for an unapplied group", got)
	}
}

// A user who collapsed a group into one broad wildcard is already covered;
// re-adding the narrow patterns underneath it would be noise.
func TestMissingGroupPatterns_BroaderRuleCoversNarrowPattern(t *testing.T) {
	group := []string{"*.booking.com", "www.booking.com", "*.bstatic.com"}
	stored := rulesFrom(routed("proxy:main"), "*.booking.com", "*.bstatic.com")

	if got := missingGroupPatterns(group, stored); got != nil {
		t.Fatalf("missing = %v, want nil (rule *.booking.com covers www.booking.com)", got)
	}
}

// Synced rules must inherit the binding the user picked originally — the
// whole point of sync over apply is not re-asking for the proxy.
func TestGroupRuleShape_TakesTheDominantBinding(t *testing.T) {
	group := []string{"*.airbnb.com", "*.muscache.com", "*.abnb.me"}
	stored := rulesFrom(routed("xray:tokyo"), "*.airbnb.com", "*.muscache.com")
	// One hand-edited outlier must not win over the two matching rules.
	stored = append(stored, rules.Rule{Pattern: "*.abnb.me", Action: rules.ActionBlock, Enabled: true})

	got, ok := groupRuleShape(group, stored)
	if !ok {
		t.Fatal("expected a shape for an applied group")
	}
	if got.Action != rules.ActionRoute || got.Interface != "xray:tokyo" || !got.Enabled {
		t.Fatalf("shape = %+v, want route via xray:tokyo, enabled", got)
	}
}

func TestGroupRuleShape_NoRulesMeansNoShape(t *testing.T) {
	if _, ok := groupRuleShape([]string{"*.pandora.com"}, nil); ok {
		t.Fatal("expected ok=false when the group has no rules")
	}
}
