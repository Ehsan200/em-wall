package main

import (
	"context"
	"fmt"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
)

// Group drift: a group's pattern list is baked into the app (or fetched, for
// dynamic groups), while the rules created from it live in the DB forever.
// So every release that adds a domain to a curated group leaves the users who
// already applied that group one domain short — the new host resolves outside
// the proxy and silently leaks. The sync path below closes that gap without
// asking the user to re-pick the action/interface they chose originally.

// groupPatternApplied reports whether some stored rule already covers a
// group pattern. Two ways to be covered:
//
//	exact      rule "*.airbnb.net"  covers group "*.airbnb.net"
//	broader    rule "*.airbnb.com"  covers group "api.airbnb.com"
//
// The second case matters because a user may have collapsed several group
// patterns into one wildcard by hand; re-adding the narrow ones would be
// noise, not a fix.
func groupPatternApplied(groupPat string, stored []rules.Rule) bool {
	for _, r := range stored {
		// Args are swapped relative to ruleBelongsToGroup on purpose: here
		// the *rule* is the scope and the group pattern is the candidate.
		if ruleCoveredByGroupPattern(groupPat, r.Pattern) {
			return true
		}
	}
	return false
}

// missingGroupPatterns returns the group patterns no stored rule covers,
// in registry order. Returns nil when the group was never applied (no rule
// belongs to it) — an untouched group is not "drifted", it's just off, and
// surfacing it as needing a sync would flag every group in the registry.
func missingGroupPatterns(groupPats []string, stored []rules.Rule) []string {
	applied := false
	for _, r := range stored {
		if ruleBelongsToGroup(r.Pattern, groupPats) {
			applied = true
			break
		}
	}
	if !applied {
		return nil
	}
	var missing []string
	for _, p := range groupPats {
		if !groupPatternApplied(p, stored) {
			missing = append(missing, p)
		}
	}
	return missing
}

// groupRuleShape infers the action/interface/enabled to give newly synced
// rules by taking the most common combination among the group's existing
// rules. Ties break toward the shape seen first (registry/insert order), so
// a group applied once as "route via proxy:X" keeps routing via proxy:X even
// if the user later blocked one of its domains by hand.
func groupRuleShape(groupPats []string, stored []rules.Rule) (rules.Rule, bool) {
	type shape struct {
		action  rules.Action
		iface   string
		enabled bool
	}
	counts := map[shape]int{}
	var order []shape
	for _, r := range stored {
		if !ruleBelongsToGroup(r.Pattern, groupPats) {
			continue
		}
		s := shape{action: r.Action, iface: r.Interface, enabled: r.Enabled}
		if counts[s] == 0 {
			order = append(order, s)
		}
		counts[s]++
	}
	if len(order) == 0 {
		return rules.Rule{}, false
	}
	best := order[0]
	for _, s := range order[1:] {
		if counts[s] > counts[best] {
			best = s
		}
	}
	return rules.Rule{Action: best.action, Interface: best.iface, Enabled: best.enabled}, true
}

// syncGroup inserts the group patterns that no stored rule covers, using the
// shape of the group's existing rules (or the caller's override). It is the
// add-only half of groups.apply: nothing is deleted, nothing is rewritten,
// and a group with no drift is a no-op.
func (d *handlerDeps) syncGroup(ctx context.Context, p ipc.GroupsSyncParams) (ipc.GroupsApplyResult, error) {
	out := ipc.GroupsApplyResult{}
	pats, err := d.resolveGroupPatterns(ctx, p.Key)
	if err != nil {
		return out, err
	}
	stored, err := d.store.List(ctx)
	if err != nil {
		return out, err
	}
	missing := missingGroupPatterns(pats, stored)
	if len(missing) == 0 {
		return out, nil
	}
	tmpl, ok := groupRuleShape(pats, stored)
	if !ok {
		// missingGroupPatterns already guarantees the group is applied, so
		// this is unreachable in practice — guard anyway rather than
		// inserting rules with a zero action.
		return out, fmt.Errorf("group %s is not applied; use apply instead of sync", p.Key)
	}
	if p.Action != "" {
		tmpl.Action = rules.Action(p.Action)
		tmpl.Interface = p.Interface
	}
	for _, pattern := range missing {
		r := tmpl
		r.Pattern = pattern
		added, err := d.store.Add(ctx, r)
		if err != nil {
			if err.Error() == rules.ErrDuplicate.Error() {
				out.Skipped = append(out.Skipped, pattern)
				continue
			}
			return out, err
		}
		out.Created = append(out.Created, ruleToDTO(added))
	}
	if len(out.Created) > 0 {
		_ = d.engine.Reload(ctx)
		// Same reason as groups.apply: IP/CIDR patterns need their kernel
		// static route installed, and Reload only refreshes the rule cache.
		d.reconcileIPRoutes(ctx)
	}
	return out, nil
}
