package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// Subscriptions as a rule exit — routing side (docs/xray-sub-exit.md).
//
// A rule "xraysub:NAME" expands (xray.Store.SubExpansions) to the hidden
// proxy row _xsub_ID, which the supervisor points at a slot whose members
// are the subscription's nodes. The node is the exit, so the xray balancer
// — shortlist, parking, path probes, strategy — picks the exit exactly as
// it picks a master's hop. This file finds which subscriptions rules use
// and brings their slots up or down when that changes; exit measurement
// lives in xray_subexit_probe.go.

// ruleExitNames returns the lower-case subscription names enabled route
// rules use as an exit.
func ruleExitNames(list []rules.Rule) map[string]bool {
	out := map[string]bool{}
	for _, r := range list {
		if !r.Enabled || r.Action != rules.ActionRoute {
			continue
		}
		if n := xray.ParseSubName(r.Interface); n != "" {
			out[n] = true
		}
	}
	return out
}

// storeRuleExits adapts the rule store to xraySupervisor.ruleExits.
func storeRuleExits(store *rules.Store) func(context.Context) map[string]bool {
	return func(ctx context.Context) map[string]bool {
		list, err := store.List(ctx)
		if err != nil {
			return nil
		}
		return ruleExitNames(list)
	}
}

// exitWatch reconciles xray when the set of subscriptions used as an exit
// changes, so a rule bound to a subscription gets its slot (and its hidden
// proxy row) before the new rule index goes live. Wired as the decision
// engine's reload hook: every rule write ends in a Reload.
type exitWatch struct {
	mu        sync.Mutex
	last      string
	reconcile func(context.Context) error
	logf      func(string, ...any)
}

func (w *exitWatch) onReload(ctx context.Context, raw []rules.Rule) {
	names := ruleExitNames(raw)
	keys := make([]string, 0, len(names))
	for n := range names {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	cur := strings.Join(keys, ",")

	w.mu.Lock()
	defer w.mu.Unlock()
	if cur == w.last {
		return
	}
	w.last = cur
	if err := w.reconcile(ctx); err != nil && w.logf != nil {
		w.logf("xray supervisor: subscription exits changed (%s), reconcile: %v", cur, err)
	}
}

// exitProbes lists every active node of every enabled subscription as a
// probe-only outbound, so the exit prober can measure any subscription —
// before a rule uses it, and including nodes parked out of their slot.
func (s *xraySupervisor) exitProbes(ctx context.Context) []xray.ExitProbe {
	subs, err := s.xrayStore.ListSubs(ctx)
	if err != nil {
		return nil
	}
	var out []xray.ExitProbe
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		nodes, err := s.xrayStore.ActiveNodesForSubs(ctx, []string{sub.Name})
		if err != nil {
			continue
		}
		for _, n := range nodes {
			out = append(out, xray.ExitProbe{SubID: sub.ID, Key: n.Fingerprint, Outbound: json.RawMessage(n.Outbound)})
		}
	}
	return out
}
