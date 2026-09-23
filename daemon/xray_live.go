package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Live config application.
//
// Restarting xray drops every connection through every entry, so a config
// change must reach the running process as the smallest set of API calls
// that reproduces it: inbounds and outbounds are replaced by tag, and the
// routing section (rules + balancers) is swapped whole — `xray api
// adrules` without -append replaces it atomically, and routing is only
// consulted when a connection is dispatched, so live streams never notice.
// Connections through an entry that did not change are not touched at all.
//
// Only a change outside those three sections (log, api, policy, stats,
// observatory) or to the first outbound — xray's default for unmatched
// traffic, which the API can't reorder — still needs a restart. Those are
// structural and rare; everything a user does day to day (add, edit,
// enable or delete an entry, add a master, a subscription refresh moving
// pool members, a routing-rule edit) applies live.

// liveConfig is a generated xray config decomposed into the units the API
// can replace independently. Every value is canonical JSON (keys sorted),
// so equal configs compare equal regardless of formatting.
type liveConfig struct {
	fixed     string            // every top-level key except inbounds/outbounds/routing
	inbounds  map[string]string // tag → object
	outbounds map[string]string // tag → object
	firstOut  string            // tag of the first outbound
	routing   string
}

func parseLiveConfig(raw []byte) (liveConfig, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return liveConfig{}, err
	}
	lc := liveConfig{inbounds: map[string]string{}, outbounds: map[string]string{}}
	fixed := map[string]json.RawMessage{}
	for k, v := range top {
		switch k {
		case "inbounds", "outbounds", "routing":
		default:
			fixed[k] = v
		}
	}
	var err error
	if lc.fixed, err = canonicalJSON(fixed); err != nil {
		return liveConfig{}, err
	}
	if lc.routing, err = canonicalJSON(top["routing"]); err != nil {
		return liveConfig{}, err
	}
	var order []string
	if order, err = taggedObjects(top["inbounds"], lc.inbounds); err != nil {
		return liveConfig{}, fmt.Errorf("inbounds: %w", err)
	}
	if order, err = taggedObjects(top["outbounds"], lc.outbounds); err != nil {
		return liveConfig{}, fmt.Errorf("outbounds: %w", err)
	}
	if len(order) > 0 {
		lc.firstOut = order[0]
	}
	return lc, nil
}

// taggedObjects fills into with tag → canonical object and returns the
// tags in array order. Untagged or duplicate-tagged objects can't be
// addressed through the API, so they are an error.
func taggedObjects(raw json.RawMessage, into map[string]string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	order := make([]string, 0, len(items))
	for _, it := range items {
		var t struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(it, &t); err != nil {
			return nil, err
		}
		if t.Tag == "" {
			return nil, fmt.Errorf("object without a tag")
		}
		if _, dup := into[t.Tag]; dup {
			return nil, fmt.Errorf("duplicate tag %q", t.Tag)
		}
		c, err := canonicalJSON(it)
		if err != nil {
			return nil, err
		}
		into[t.Tag] = c
		order = append(order, t.Tag)
	}
	return order, nil
}

// canonicalJSON re-encodes v with object keys sorted (encoding/json sorts
// map keys), so formatting differences never read as a change.
func canonicalJSON(v any) (string, error) {
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return "", nil
		}
		var anyV any
		if err := json.Unmarshal(raw, &anyV); err != nil {
			return "", err
		}
		v = anyV
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// livePlan is the API work that turns one config into another.
type livePlan struct {
	restart bool // a change the API can't express

	rmOut, addOut []string // tags; a changed outbound is in both
	rmIn, addIn   []string
	routing       bool
}

func (p livePlan) empty() bool {
	return !p.restart && !p.routing && len(p.rmOut)+len(p.addOut)+len(p.rmIn)+len(p.addIn) == 0
}

func planLive(old, want liveConfig) livePlan {
	if old.fixed != want.fixed || old.firstOut != want.firstOut {
		return livePlan{restart: true}
	}
	var p livePlan
	p.rmOut, p.addOut = diffTagged(old.outbounds, want.outbounds)
	p.rmIn, p.addIn = diffTagged(old.inbounds, want.inbounds)
	p.routing = old.routing != want.routing
	return p
}

// diffTagged returns the tags to remove (gone or changed) and to add (new
// or changed), each sorted for deterministic API calls.
func diffTagged(old, want map[string]string) (rm, add []string) {
	for tag, o := range old {
		if w, ok := want[tag]; !ok || w != o {
			rm = append(rm, tag)
		}
	}
	for tag, w := range want {
		if o, ok := old[tag]; !ok || w != o {
			add = append(add, tag)
		}
	}
	sort.Strings(rm)
	sort.Strings(add)
	return rm, add
}

// applyLiveLocked moves the running process from the config it was started
// with (or last brought to) to want, through the API. Any failure returns
// an error and the caller falls back to a restart, which converges from
// any intermediate state. Caller holds s.mu.
//
// Order keeps every reference resolvable: outbounds first (routing and
// dialerProxy name them), then inbounds, then routing, and only then the
// removals of things nothing references any more. A changed object is
// removed and re-added under the same tag — only connections through that
// one object are affected, which is the change the user asked for.
func (s *xraySupervisor) applyLiveLocked(ctx context.Context, p livePlan, want []byte) error {
	var top struct {
		Inbounds  []json.RawMessage `json:"inbounds"`
		Outbounds []json.RawMessage `json:"outbounds"`
		Routing   json.RawMessage   `json:"routing"`
	}
	if err := json.Unmarshal(want, &top); err != nil {
		return err
	}
	pick := func(items []json.RawMessage, tags []string) []json.RawMessage {
		set := make(map[string]bool, len(tags))
		for _, t := range tags {
			set[t] = true
		}
		var out []json.RawMessage
		for _, it := range items {
			var t struct {
				Tag string `json:"tag"`
			}
			if json.Unmarshal(it, &t) == nil && set[t.Tag] {
				out = append(out, it)
			}
		}
		return out
	}
	addOut := toSet(p.addOut)
	addIn := toSet(p.addIn)
	changedOut := intersect(p.rmOut, addOut)
	changedIn := intersect(p.rmIn, addIn)

	if len(changedOut) > 0 {
		if _, err := s.runAPI(ctx, "rmo", changedOut...); err != nil {
			return err
		}
	}
	if len(p.addOut) > 0 {
		if err := s.apiWithDoc(ctx, "ado", map[string]any{"outbounds": pick(top.Outbounds, p.addOut)}); err != nil {
			return err
		}
	}
	if len(changedIn) > 0 {
		if _, err := s.runAPI(ctx, "rmi", changedIn...); err != nil {
			return err
		}
	}
	if len(p.addIn) > 0 {
		if err := s.apiWithDoc(ctx, "adi", map[string]any{"inbounds": pick(top.Inbounds, p.addIn)}); err != nil {
			return err
		}
	}
	if p.routing {
		// No -append: replaces the rules AND balancers wholesale.
		if err := s.apiWithDoc(ctx, "adrules", map[string]any{"routing": top.Routing}); err != nil {
			return err
		}
	}
	if gone := minus(p.rmIn, addIn); len(gone) > 0 {
		if _, err := s.runAPI(ctx, "rmi", gone...); err != nil {
			return err
		}
	}
	if gone := minus(p.rmOut, addOut); len(gone) > 0 {
		if _, err := s.runAPI(ctx, "rmo", gone...); err != nil {
			return err
		}
	}
	return nil
}

// apiWithDoc writes doc to a temp file in the runtime dir and hands it to
// `xray api <sub>`, which only accepts config files.
func (s *xraySupervisor) apiWithDoc(ctx context.Context, sub string, doc any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.runtimeDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.runtimeDir, "api-"+sub+"-*.json")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = s.runAPI(ctx, sub, filepath.Clean(path))
	return err
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func intersect(xs []string, set map[string]bool) []string {
	var out []string
	for _, x := range xs {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

func minus(xs []string, set map[string]bool) []string {
	var out []string
	for _, x := range xs {
		if !set[x] {
			out = append(out, x)
		}
	}
	return out
}
