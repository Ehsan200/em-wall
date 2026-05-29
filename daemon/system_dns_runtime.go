package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/dnsproxy"
	"github.com/ehsan/em-wall/core/ipc"
)

// systemDNSStatus snapshots the current per-service DNS, what scutil
// sees, what we're currently using as upstream, and whether we're
// active.
func (d *handlerDeps) systemDNSStatus() ipc.SystemDNSStatus {
	active, _ := d.sysDNS.IsActive()
	resolvers, _ := d.sysDNS.DetectResolvers()
	per, _ := d.sysDNS.CaptureAll()
	d.mu.Lock()
	upstream := splitCSV(d.upstream)
	d.mu.Unlock()
	return ipc.SystemDNSStatus{
		Active:            active,
		Upstream:          upstream,
		DetectedResolvers: resolvers,
		PerService:        per,
	}
}

func (d *handlerDeps) activateSystemDNS(ctx context.Context) error {
	wasActive, _ := d.sysDNS.IsActive()

	snap, err := d.sysDNS.CaptureAll()
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}

	// Sanitize snapshot for backup: a service whose DNS is *only* a
	// loopback (i.e. ourselves) should be treated as DHCP-supplied so
	// that Deactivate restores it to Empty rather than 127.0.0.1.
	clean := sanitizeSnapshot(snap)

	// If we're already active and have a saved backup, keep it — we
	// don't want to overwrite the original pre-activation state with
	// our own 127.0.0.1 entries.
	if !wasActive {
		snapJSON, err := json.Marshal(clean)
		if err != nil {
			return fmt.Errorf("marshal snapshot: %w", err)
		}
		if err := d.store.SetSetting(ctx, "system_dns_backup", string(snapJSON)); err != nil {
			return fmt.Errorf("save backup: %w", err)
		}
	}

	// Pick upstream — every candidate is validated with a live query,
	// so what comes back is a list of resolvers we KNOW respond.
	upstream := d.chooseUpstream(ctx, clean)
	if len(upstream) == 0 {
		// Last-ditch: try the fallback resolvers, but still validate.
		if working := ValidateResolvers(ctx, d.fallbackDNS(ctx)); len(working) > 0 {
			upstream = working
		}
	}
	if len(upstream) == 0 {
		// REFUSE TO ACTIVATE. Leaving 127.0.0.1 set without a working
		// upstream would brick DNS system-wide — exactly what bit us
		// before. Surface a clear error and leave system DNS alone.
		// If we were ALREADY in the 127.0.0.1 state (prior bad run, or
		// LaunchDaemon started us before the network came up), recover
		// by restoring user's DNS so DNS keeps working — but do NOT
		// flip the persisted system_dns_active preference. The user
		// still wants the hijack; the watcher will retry when the
		// network is reachable.
		if wasActive {
			log.Printf("em-walld: no working upstream — restoring system DNS (will retry)")
			_ = d.restoreSystemDNSBackup(ctx)
		}
		return fmt.Errorf("no working upstream DNS found — refusing to hijack system DNS (would break resolution for every app)")
	}
	if err := d.store.SetSetting(ctx, "upstream_dns", joinCSV(upstream)); err != nil {
		return fmt.Errorf("save upstream: %w", err)
	}

	// Swap forwarder before flipping system DNS so the very first
	// query through us has a working upstream.
	d.dnsServer.SetForwarder(dnsproxy.NewMultiUpstream(upstream, 3*time.Second))
	d.mu.Lock()
	d.upstream = joinCSV(upstream)
	d.mu.Unlock()

	if err := d.sysDNS.ApplyAll([]string{"127.0.0.1"}); err != nil {
		return fmt.Errorf("apply 127.0.0.1: %w", err)
	}
	_ = d.store.SetSetting(ctx, "system_dns_active", "true")
	flushDNSCache()
	return nil
}

// chooseUpstream collects every plausible upstream resolver, then
// validates each with a real query and returns only those that
// actually answered.
//
// Sources, in priority order (lower index wins ties after validation):
//  1. Live per-service manual values from snap (excluding loopback).
//  2. Saved pre-activation backup.
//  3. AllDHCPDNS — every non-tunnel hardware port. This is the line
//     that fixes the "VPN owns default route → ignore Wi-Fi DHCP" bug.
//  4. scutil --dns (excluding loopback).
//  5. publicFallbackDNS — always appended LAST so it only ever acts as a
//     per-name safety net behind the network-local resolvers (see below).
//
// Returns nil only if nothing validates at all (no usable network).
func (d *handlerDeps) chooseUpstream(ctx context.Context, snap map[string][]string) []string {
	seen := map[string]bool{}
	var candidates []string
	add := func(ips ...string) {
		for _, ip := range ips {
			if ip == "" || isLoopback(stripPort(ip)) {
				continue
			}
			withPort := WithPort53([]string{ip})[0]
			if seen[withPort] {
				continue
			}
			seen[withPort] = true
			candidates = append(candidates, withPort)
		}
	}

	for _, ips := range snap {
		add(ips...)
	}
	if raw, _ := d.store.GetSetting(ctx, "system_dns_backup", ""); raw != "" {
		var backup map[string][]string
		if err := json.Unmarshal([]byte(raw), &backup); err == nil {
			for _, ips := range backup {
				add(ips...)
			}
		}
	}
	if dhcp, err := d.sysDNS.AllDHCPDNS(); err == nil {
		add(dhcp...)
	}
	if det, err := d.sysDNS.DetectResolvers(); err == nil {
		add(det...)
	}

	// Always offer the public resolvers as LOWEST-priority candidates,
	// appended after every network-local one. MultiUpstream.Forward stops
	// at the first positive reply, so for any name a local resolver can
	// answer these are never even queried — privacy and latency are
	// preserved, and a public NXDOMAIN can't outrank a local positive for
	// an in-country split-DNS .ir name. They only get consulted when EVERY
	// local resolver soft-fails a name (SERVFAIL / timeout) — e.g. a
	// VPN-pushed resolver that can't resolve certain .ir CDN hosts — which
	// is exactly the gap that left those names returning SERVFAIL with no
	// recovery path. Validation below drops them on a network where they
	// can't be reached, so we never list a resolver that's actually dead.
	add(d.fallbackDNS(ctx)...)

	if len(candidates) == 0 {
		return nil
	}
	working := ValidateResolvers(ctx, candidates)
	return working
}

func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// sanitizeSnapshot drops loopback entries. A service whose only entry
// was 127.0.0.1 ends up with nil (DHCP-supplied) so restore is correct.
func sanitizeSnapshot(snap map[string][]string) map[string][]string {
	out := make(map[string][]string, len(snap))
	for svc, ips := range snap {
		var clean []string
		for _, ip := range ips {
			if !isLoopback(ip) {
				clean = append(clean, ip)
			}
		}
		out[svc] = clean
	}
	return out
}

// refreshUpstreamIfStale validates the current upstream(s) with a real
// query. If at least one still answers, no-op. Otherwise it re-picks
// via chooseUpstream and swaps the forwarder atomically. Called by
// the network watcher on a 10s tick — also safe to call on demand.
//
// Returns changed=true only when the upstream was actually swapped.
// The intent is "DNS keeps working when the user changes network":
// chooseUpstream reads AllDHCPDNS / scutil / per-service backup, all
// of which reflect the current network state, so the new upstream is
// whatever's reachable on whichever Wi-Fi/Ethernet/VPN the user moved
// to. We don't touch system DNS — 127.0.0.1 stays in every service's
// resolver list, the daemon just forwards somewhere different now.
func (d *handlerDeps) refreshUpstreamIfStale(ctx context.Context) (bool, error) {
	d.mu.Lock()
	current := splitCSV(d.upstream)
	d.mu.Unlock()

	// Cheap health check: if any current upstream still answers, leave
	// it alone. ValidateResolvers runs them in parallel with a 1.5s
	// timeout each, so this finishes well within the 10s tick.
	if len(current) > 0 {
		if working := ValidateResolvers(ctx, current); len(working) > 0 {
			return false, nil
		}
	}

	// Current upstream is dead — typically because the user moved
	// networks. Re-pick from sources that read fresh state.
	return d.repickUpstream(ctx)
}

// repickUpstream unconditionally re-runs chooseUpstream against the
// current network state and swaps the live forwarder + persisted
// upstream_dns to the result. Unlike refreshUpstreamIfStale it does NOT
// first check whether the current upstream still works — callers use it
// to force a recompute after something that changes the *chosen* list
// rather than its reachability (e.g. the user edits the fallback-DNS
// setting). Returns changed=false (no error) when the recomputed list is
// identical to what's already live, so it's cheap to call speculatively.
func (d *handlerDeps) repickUpstream(ctx context.Context) (bool, error) {
	d.mu.Lock()
	current := splitCSV(d.upstream)
	d.mu.Unlock()

	snap, err := d.sysDNS.CaptureAll()
	if err != nil {
		return false, fmt.Errorf("capture: %w", err)
	}
	clean := sanitizeSnapshot(snap)
	upstream := d.chooseUpstream(ctx, clean)
	if len(upstream) == 0 {
		// Last-ditch fallback — still validated.
		if working := ValidateResolvers(ctx, d.fallbackDNS(ctx)); len(working) > 0 {
			upstream = working
		}
	}
	if len(upstream) == 0 {
		return false, fmt.Errorf("no working upstream DNS on current network")
	}
	if joinCSV(upstream) == joinCSV(current) {
		// New pick happens to match what we already had (e.g. validation
		// flapped, or the edited fallback was unreachable). Don't churn
		// the forwarder.
		return false, nil
	}

	if err := d.store.SetSetting(ctx, "upstream_dns", joinCSV(upstream)); err != nil {
		return false, fmt.Errorf("save upstream: %w", err)
	}
	d.dnsServer.SetForwarder(dnsproxy.NewMultiUpstream(upstream, 3*time.Second))
	d.mu.Lock()
	d.upstream = joinCSV(upstream)
	d.mu.Unlock()
	return true, nil
}

// fallbackDNS returns the resolvers to append as lowest-priority upstream
// candidates: the user-configured list (settings key "fallback_dns") when
// set and valid, otherwise the built-in publicFallbackDNS. The stored
// value is validated at write time (MethodSettingsSet), so the re-parse
// here should always succeed; the publicFallbackDNS path is a defensive
// floor in case the row is somehow malformed.
func (d *handlerDeps) fallbackDNS(ctx context.Context) []string {
	raw, _ := d.store.GetSetting(ctx, "fallback_dns", "")
	if servers, bad := parseDNSServers(raw); bad == "" && len(servers) > 0 {
		return servers
	}
	return publicFallbackDNS
}

// parseDNSServers splits a user-entered fallback-DNS string into validated
// "host:53" entries. Accepts commas, semicolons, and any whitespace
// (including newlines) as separators so the UI can offer a multi-line box.
// Each token must be an IPv4/IPv6 literal, optionally with an explicit
// :port. Returns the normalized, de-duplicated list; if any non-empty
// token is not a valid address it returns (nil, thatToken) so callers can
// reject the input with a precise message. Empty input → (nil, "").
func parseDNSServers(s string) (servers []string, invalid string) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	seen := map[string]bool{}
	for _, f := range fields {
		host := f
		if h, _, err := net.SplitHostPort(f); err == nil {
			host = h
		}
		if net.ParseIP(host) == nil {
			return nil, f
		}
		withPort := WithPort53([]string{f})[0]
		if seen[withPort] {
			continue
		}
		seen[withPort] = true
		servers = append(servers, withPort)
	}
	return servers, ""
}

func (d *handlerDeps) deactivateSystemDNS(ctx context.Context) error {
	if err := d.restoreSystemDNSBackup(ctx); err != nil {
		return err
	}
	_ = d.store.SetSetting(ctx, "system_dns_active", "false")
	return nil
}

// restoreSystemDNSBackup applies the saved pre-hijack DNS snapshot (or
// clears every service if no backup exists) and flushes the resolver
// cache. It does NOT touch the system_dns_active preference — the caller
// owns that. Use this when restoring DNS as a safety measure during a
// failed activation, where the user's intent ("hijack on") should remain
// recorded so the watcher can retry.
func (d *handlerDeps) restoreSystemDNSBackup(ctx context.Context) error {
	raw, err := d.store.GetSetting(ctx, "system_dns_backup", "")
	if err != nil {
		return err
	}
	if raw != "" {
		var snap map[string][]string
		if err := json.Unmarshal([]byte(raw), &snap); err != nil {
			return fmt.Errorf("parse backup: %w", err)
		}
		if err := d.sysDNS.RestoreAll(snap); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	} else {
		services, err := d.sysDNS.ListServices()
		if err != nil {
			return err
		}
		for _, svc := range services {
			_ = d.sysDNS.SetServiceDNS(svc, nil)
		}
	}
	flushDNSCache()
	return nil
}

// flushDNSCache tells macOS to discard its resolver cache after a
// system DNS change so stale pre-change answers don't keep being served.
func flushDNSCache() {
	exec.Command("dscacheutil", "-flushcache").Run()
	exec.Command("killall", "-HUP", "mDNSResponder").Run()
}
