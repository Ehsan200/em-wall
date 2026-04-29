// em-walld is the privileged firewall daemon. It runs as root (via
// LaunchDaemon), owns the SQLite rule store, runs the DNS proxy on
// 127.0.0.1:53, manages per-host routes, and exposes an IPC socket
// for the Wails UI to drive it.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ehsan/em-wall/core/applocator"
	"github.com/ehsan/em-wall/core/decision"
	"github.com/ehsan/em-wall/core/dnsproxy"
	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/pfctl"
	"github.com/ehsan/em-wall/core/routing"
	"github.com/ehsan/em-wall/core/rules"
)

// lsofProvider adapts core/routing's exported LsofUtunOwners to the
// LsofProvider interface that applocator depends on. Keeps applocator
// free of any direct dependency on routing.
type lsofProvider struct{}

func (lsofProvider) LsofUtunOwners() map[string]string { return routing.LsofUtunOwners() }

const Version = "0.1.0"

func main() {
	var (
		dbPath         = flag.String("db", "/usr/local/var/em-wall/rules.db", "path to SQLite database")
		sockPath       = flag.String("socket", ipc.DefaultSocketPath, "path to IPC unix socket")
		listenAddr     = flag.String("listen", "127.0.0.1:53", "DNS proxy listen address")
		upstream       = flag.String("upstream", "1.1.1.1:53,8.8.8.8:53", "comma-separated upstream DNS servers")
		noAutoActivate = flag.Bool("no-auto-activate", false, "do not touch system DNS on startup (for tests / dev)")
	)
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("em-walld: mkdir db dir: %v", err)
	}

	store, err := rules.Open(*dbPath)
	if err != nil {
		log.Fatalf("em-walld: open store: %v", err)
	}
	defer store.Close()

	engine := decision.New(store)
	if err := engine.Reload(context.Background()); err != nil {
		log.Fatalf("em-walld: load rules: %v", err)
	}

	router := routing.New(nil)
	pf := pfctl.New(nil)
	sysDNS := NewSystemDNS(nil)
	apps := applocator.NewResolver(lsofProvider{})
	apps.Refresh() // populate initial app→utun mapping

	logSink := &storeLogSink{store: store}

	// Pick upstream: stored setting > flag default. The setting is
	// populated when the user clicks "Activate" in the UI, capturing
	// whatever the system was using before we hijacked it.
	upstreams := loadUpstream(store, *upstream)
	fwd := dnsproxy.NewMultiUpstream(upstreams, 3*time.Second)

	dnsServer, err := dnsproxy.New(dnsproxy.Config{
		Listen:     *listenAddr,
		Decider:    engine,
		Forwarder:  fwd,
		Routes:     router,
		Interfaces: dnsproxy.DefaultInterfaceChecker,
		Apps:       apps,
		Logs:       logSink,
		Logger:     log.Default(),
	})
	if err != nil {
		log.Fatalf("em-walld: dnsproxy: %v", err)
	}

	ipcSrv := ipc.NewServer(*sockPath, log.Default())
	deps := &handlerDeps{
		store:      store,
		engine:     engine,
		router:     router,
		pf:         pf,
		sysDNS:     sysDNS,
		dnsServer:  dnsServer,
		apps:       apps,
		listenAddr: *listenAddr,
		upstream:   joinCSV(upstreams),
		startedAt:  time.Now(),
	}
	registerHandlers(ipcSrv, deps)

	// Restore pf state from settings.
	if v, _ := store.GetSetting(context.Background(), "block_encrypted_dns", "false"); v == "true" {
		if err := pf.Enable(context.Background()); err != nil {
			log.Printf("em-walld: pf enable failed (continuing): %v", err)
		}
	}

	// Auto-activate the DNS hijack unless explicitly disabled (via flag
	// or persisted setting). Default-on so the daemon is useful out of
	// the box; activation now validates upstream and refuses to brick
	// DNS if nothing works.
	if *noAutoActivate {
		log.Printf("em-walld: -no-auto-activate set, leaving system DNS alone")
	} else if v, _ := store.GetSetting(context.Background(), "system_dns_active", "true"); v == "true" {
		if err := deps.activateSystemDNS(context.Background()); err != nil {
			log.Printf("em-walld: auto-activate failed (continuing): %v", err)
		} else {
			log.Printf("em-walld: system DNS hijack active")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(5)

	go func() {
		defer wg.Done()
		log.Printf("em-walld: dns proxy listening on %s", *listenAddr)
		if err := dnsServer.Start(ctx); err != nil {
			log.Printf("em-walld: dns proxy stopped: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		log.Printf("em-walld: ipc socket at %s", *sockPath)
		if err := ipcSrv.Serve(ctx); err != nil {
			log.Printf("em-walld: ipc stopped: %v", err)
		}
	}()

	// Periodic route TTL sweeper.
	go func() {
		defer wg.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				router.SweepExpired(ctx)
			}
		}
	}()

	// App watcher: 1s tick. On each detected change, take the per-app
	// write-lock (queries for that app block briefly), flush stale
	// per-host routes pinned to the old utun, then release. The next
	// query installs fresh routes via the new utun. New rules are
	// picked up automatically by the next query (engine cache).
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				changes := apps.Refresh()
				for _, c := range changes {
					release := apps.AcquireForWrite(c.Key)
					if c.Old != "" {
						_ = router.RemoveByInterface(ctx, c.Old)
					}
					log.Printf("em-walld: app %s: %s → %s", c.Key,
						orDash(c.Old), orDash(c.New))
					release()
				}
			}
		}
	}()

	// Upstream watcher: every 10s, validate that the current upstream
	// still answers. When the user switches Wi-Fi, sleeps/wakes the
	// laptop, or toggles a VPN, the DHCP-supplied resolver we picked at
	// activation time is often no longer reachable — every query hangs
	// or NXDOMAIN's because we're forwarding into the void. The fix is
	// to notice and re-pick atomically: chooseUpstream reads
	// AllDHCPDNS, which queries `ipconfig getoption en0
	// domain_name_server` live, so it reflects whichever network we're
	// on now. Forwarder swap is atomic — no daemon restart, no
	// DNS-down window for the user.
	go func() {
		defer wg.Done()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if changed, err := deps.refreshUpstreamIfStale(ctx); err != nil {
					log.Printf("em-walld: upstream refresh failed: %v", err)
				} else if changed {
					log.Printf("em-walld: upstream refreshed: now using %s", deps.upstream)
				}
			}
		}
	}()

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("em-walld: shutting down")

	cancel()
	dnsServer.Shutdown()
	ipcSrv.Shutdown()
	router.Flush(context.Background())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("em-walld: shutdown timeout")
	}
	log.Printf("em-walld: bye")
}

type storeLogSink struct {
	store *rules.Store
}

func (s *storeLogSink) Log(name, action, iface string, ruleID int64, clientIP string) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.store.Log(ctx, rules.LogEntry{
		QueryName: name,
		Action:    action,
		RuleID:    ruleID,
		Interface: iface,
		ClientIP:  clientIP,
	})
}

// ---------- IPC handler wiring ----------

type handlerDeps struct {
	store      *rules.Store
	engine     *decision.Engine
	router     *routing.Manager
	pf         *pfctl.Manager
	sysDNS     *SystemDNS
	dnsServer  *dnsproxy.Server
	apps       *applocator.Resolver
	listenAddr string
	upstream   string
	startedAt  time.Time

	mu sync.Mutex // guards upstream
}

// loadUpstream picks the daemon's startup forwarder list. Loopback is
// stripped at every layer — under no circumstance should we forward
// queries to ourselves and create a tight loop. The picked list is
// validated lazily by activateSystemDNS before being applied.
//
// Order:
//  1. settings.upstream_dns (set by Activate, captured pre-hijack)
//  2. flagDefault (CLI flag)
//  3. AllDHCPDNS across every non-tunnel hardware port
//  4. scutil --dns (excluding loopback)
//  5. public fallback (1.1.1.1, 8.8.8.8) — last resort
func loadUpstream(store *rules.Store, flagDefault string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if v, _ := store.GetSetting(ctx, "upstream_dns", ""); v != "" {
		if clean := stripLoopback(splitCSV(v)); len(clean) > 0 {
			return clean
		}
	}
	if flagDefault != "" {
		if clean := stripLoopback(splitCSV(flagDefault)); len(clean) > 0 {
			return clean
		}
	}
	sd := NewSystemDNS(nil)
	if ips, err := sd.AllDHCPDNS(); err == nil && len(ips) > 0 {
		return WithPort53(ips)
	}
	if ips, err := sd.DetectResolvers(); err == nil && len(ips) > 0 {
		return WithPort53(ips)
	}
	return []string{"1.1.1.1:53", "8.8.8.8:53"}
}

// stripLoopback drops 127.* / ::1 entries from a list of host[:port]
// strings. Empty list → empty list (caller decides fallback).
func stripLoopback(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			host = a
		}
		if isLoopback(host) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinCSV(parts []string) string { return strings.Join(parts, ",") }

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func registerHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodStatus, func(ctx context.Context, _ json.RawMessage) (any, error) {
		list, _ := d.store.List(ctx)
		blockEnc, _ := d.store.GetSetting(ctx, "block_encrypted_dns", "false")
		return ipc.StatusResult{
			Version:           Version,
			Uptime:            time.Since(d.startedAt).Round(time.Second).String(),
			BlockEncryptedDNS: blockEnc == "true",
			UpstreamDNS:       d.upstream,
			ListenAddr:        d.listenAddr,
			RuleCount:         len(list),
		}, nil
	})

	s.Handle(ipc.MethodRulesList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		list, err := d.store.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.RuleDTO, len(list))
		for i, r := range list {
			out[i] = ruleToDTO(r)
		}
		return out, nil
	})

	s.Handle(ipc.MethodRulesAdd, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RulesAddParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		r := rules.Rule{
			Pattern:   p.Pattern,
			Action:    rules.Action(p.Action),
			Interface: p.Interface,
			Enabled:   p.Enabled,
		}
		added, err := d.store.Add(ctx, r)
		if err != nil {
			return nil, err
		}
		_ = d.engine.Reload(ctx)
		return ruleToDTO(added), nil
	})

	s.Handle(ipc.MethodRulesUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RulesUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		r := rules.Rule{
			ID:        p.ID,
			Pattern:   p.Pattern,
			Action:    rules.Action(p.Action),
			Interface: p.Interface,
			Enabled:   p.Enabled,
		}
		if err := d.store.Update(ctx, r); err != nil {
			return nil, err
		}
		// Flush per-host routes installed for this rule. The next DNS
		// query will reinstall them via the new binding (or not, if
		// the rule is now disabled / now points elsewhere). Without
		// this, switching a rule from utun4 to app:tailscale would
		// leave the original utun4 routes in the OS table — letting
		// browser-cached IPs reach the destination via the wrong path.
		_ = d.router.RemoveByRule(ctx, p.ID)
		_ = d.engine.Reload(ctx)
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodRulesDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RulesDeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		_ = d.router.RemoveByRule(ctx, p.ID)
		if err := d.store.Delete(ctx, p.ID); err != nil {
			return nil, err
		}
		_ = d.engine.Reload(ctx)
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodSettingsGet, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.SettingsGetParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		v, err := d.store.GetSetting(ctx, p.Key, p.Default)
		if err != nil {
			return nil, err
		}
		return map[string]string{"value": v}, nil
	})

	s.Handle(ipc.MethodSettingsSet, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.SettingsSetParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.store.SetSetting(ctx, p.Key, p.Value); err != nil {
			return nil, err
		}
		// Side-effect: keep pf in sync with the toggle.
		if p.Key == "block_encrypted_dns" {
			if err := d.pf.Sync(ctx, p.Value == "true"); err != nil {
				return nil, fmt.Errorf("pf sync: %w", err)
			}
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodLogsRecent, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.LogsRecentParams
		_ = json.Unmarshal(raw, &p)
		list, err := d.store.RecentLogs(ctx, p.Limit, p.Filter)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.LogDTO, len(list))
		for i, e := range list {
			out[i] = ipc.LogDTO{
				ID:        e.ID,
				Timestamp: e.Timestamp.Format(time.RFC3339),
				QueryName: e.QueryName,
				Action:    e.Action,
				RuleID:    e.RuleID,
				Interface: e.Interface,
				ClientIP:  e.ClientIP,
			}
		}
		return out, nil
	})

	s.Handle(ipc.MethodRoutesActive, func(_ context.Context, _ json.RawMessage) (any, error) {
		active := d.router.Active()
		out := make([]ipc.ActiveRouteDTO, len(active))
		for i, a := range active {
			out[i] = ipc.ActiveRouteDTO{
				Host:      a.Host,
				Interface: a.Interface,
				ExpiresAt: a.ExpiresAt.Format(time.RFC3339),
				RuleID:    a.RuleID,
			}
		}
		return out, nil
	})

	s.Handle(ipc.MethodInterfacesList, func(_ context.Context, _ json.RawMessage) (any, error) {
		list, err := routing.EnumerateInterfaces()
		if err != nil {
			return nil, err
		}
		out := make([]ipc.InterfaceDTO, len(list))
		for i, ifc := range list {
			out[i] = ipc.InterfaceDTO{
				Name:  ifc.Name,
				Index: ifc.Index,
				MTU:   ifc.MTU,
				Flags: ifc.Flags,
				Owner: ifc.Owner,
			}
		}
		return out, nil
	})

	s.Handle(ipc.MethodSystemRoutesList, func(_ context.Context, _ json.RawMessage) (any, error) {
		list, err := routing.ListSystemRoutes()
		if err != nil {
			return nil, err
		}
		out := make([]ipc.SystemRouteDTO, len(list))
		for i, r := range list {
			out[i] = ipc.SystemRouteDTO{
				Family:      r.Family,
				Destination: r.Destination,
				Gateway:     r.Gateway,
				Flags:       r.Flags,
				Interface:   r.Interface,
			}
		}
		return out, nil
	})

	s.Handle(ipc.MethodAppsList, func(_ context.Context, _ json.RawMessage) (any, error) {
		registry := d.apps.Apps()
		out := make([]ipc.AppDTO, 0, len(registry))
		for _, a := range registry {
			path := a.InstalledPath()
			if path == "" {
				path = a.BundlePath // fall back to primary so UI has SOMETHING to show
			}
			out = append(out, ipc.AppDTO{
				Key:          a.Key,
				DisplayName:  a.DisplayName,
				BundleID:     a.BundleID,
				BundlePath:   path,
				Installed:    a.IsInstalled(),
				CurrentIface: d.apps.Current(a.Key),
			})
		}
		return out, nil
	})

	s.Handle(ipc.MethodAppsIcon, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p ipc.AppsIconParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		a := applocator.FindByKey(p.Key)
		if a == nil {
			return nil, fmt.Errorf("unknown app: %s", p.Key)
		}
		icon := applocator.LoadIcon(*a)
		return ipc.AppIconDTO{
			Key:       a.Key,
			MIME:      icon.MIME,
			DataB64:   base64.StdEncoding.EncodeToString(icon.Data),
			Installed: icon.Installed,
		}, nil
	})

	s.Handle(ipc.MethodGroupsList, func(_ context.Context, _ json.RawMessage) (any, error) {
		registry := groups.KnownGroups()
		out := make([]ipc.GroupDTO, 0, len(registry))
		for _, g := range registry {
			out = append(out, ipc.GroupDTO{
				Key:         g.Key,
				DisplayName: g.DisplayName,
				Description: g.Description,
				Patterns:    g.Patterns,
			})
		}
		return out, nil
	})

	s.Handle(ipc.MethodGroupsApply, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsApplyParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		g := groups.FindByKey(p.Key)
		if g == nil {
			return nil, fmt.Errorf("unknown group: %s", p.Key)
		}
		// Validate the action / interface combination once up front so
		// we don't half-create the group on a bad request.
		probe := rules.Rule{
			Pattern: g.Patterns[0], Action: rules.Action(p.Action),
			Interface: p.Interface, Enabled: p.Enabled,
		}
		if _, err := d.store.Add(ctx, probe); err != nil {
			// Rollback the probe insertion if it actually went through
			// (validation passed but it was a real Add). Either way we
			// return the error so the UI can show it.
			if probe.ID > 0 {
				_ = d.store.Delete(ctx, probe.ID)
			}
			// If the only error is duplicate-pattern, don't bail — that
			// pattern will just be skipped below.
			if err.Error() != rules.ErrDuplicate.Error() {
				return nil, err
			}
		}
		// Now insert the rest, tracking created vs skipped.
		out := ipc.GroupsApplyResult{}
		if probe.ID > 0 {
			out.Created = append(out.Created, ruleToDTO(probe))
		} else if probe.ID == 0 {
			out.Skipped = append(out.Skipped, g.Patterns[0])
		}
		for _, pattern := range g.Patterns[1:] {
			r := rules.Rule{
				Pattern: pattern, Action: rules.Action(p.Action),
				Interface: p.Interface, Enabled: p.Enabled,
			}
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
		_ = d.engine.Reload(ctx)
		return out, nil
	})

	// Bulk delete every rule that came from a group's pattern list.
	// Match is normalized-equality of pattern (groups.go patterns are
	// canonical lowercase). Rules the user hand-edited won't match
	// anymore — that's the intended behavior, edits opt out of group
	// membership.
	s.Handle(ipc.MethodGroupsDeleteRules, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsDeleteRulesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		g := groups.FindByKey(p.Key)
		if g == nil {
			return nil, fmt.Errorf("unknown group: %s", p.Key)
		}
		ids, err := d.matchingRuleIDs(ctx, g.Patterns)
		if err != nil {
			return nil, err
		}
		var deleted []int64
		for _, id := range ids {
			_ = d.router.RemoveByRule(ctx, id)
			if err := d.store.Delete(ctx, id); err != nil {
				continue
			}
			deleted = append(deleted, id)
		}
		_ = d.engine.Reload(ctx)
		return ipc.GroupsBulkResult{Affected: len(deleted), RuleIDs: deleted}, nil
	})

	// Bulk enable/disable. Same matching rule as delete. We re-use
	// store.Update so per-rule normalization runs (e.g. action/interface
	// validation), but the only field that's actually changing is
	// enabled. Disabled rules also get their per-host routes flushed —
	// otherwise traffic could keep using cached pinned routes after
	// the rule "stops" mattering.
	s.Handle(ipc.MethodGroupsSetEnabled, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsSetEnabledParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		g := groups.FindByKey(p.Key)
		if g == nil {
			return nil, fmt.Errorf("unknown group: %s", p.Key)
		}
		all, err := d.store.List(ctx)
		if err != nil {
			return nil, err
		}
		var touched []int64
		for _, r := range all {
			if !ruleBelongsToGroup(r.Pattern, g.Patterns) {
				continue
			}
			if r.Enabled == p.Enabled {
				continue // already in the desired state
			}
			r.Enabled = p.Enabled
			if err := d.store.Update(ctx, r); err != nil {
				continue
			}
			if !p.Enabled {
				_ = d.router.RemoveByRule(ctx, r.ID)
			}
			touched = append(touched, r.ID)
		}
		_ = d.engine.Reload(ctx)
		return ipc.GroupsBulkResult{Affected: len(touched), RuleIDs: touched}, nil
	})

	s.Handle(ipc.MethodGroupsIcon, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsIconParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		g := groups.FindByKey(p.Key)
		if g == nil {
			return nil, fmt.Errorf("unknown group: %s", p.Key)
		}
		icon := groups.LoadIcon(*g)
		return ipc.GroupIconDTO{
			Key:     g.Key,
			MIME:    icon.MIME,
			DataB64: base64.StdEncoding.EncodeToString(icon.Data),
		}, nil
	})

	s.Handle(ipc.MethodReload, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := d.engine.Reload(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodSystemDNSStatus, func(_ context.Context, _ json.RawMessage) (any, error) {
		return d.systemDNSStatus(), nil
	})

	s.Handle(ipc.MethodSystemDNSActivate, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := d.activateSystemDNS(ctx); err != nil {
			return nil, err
		}
		return d.systemDNSStatus(), nil
	})

	s.Handle(ipc.MethodSystemDNSDeactivate, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := d.deactivateSystemDNS(ctx); err != nil {
			return nil, err
		}
		return d.systemDNSStatus(), nil
	})
}

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
		// Last-ditch: try public fallback, but still validate.
		if working := ValidateResolvers(ctx, []string{"1.1.1.1:53", "8.8.8.8:53"}); len(working) > 0 {
			upstream = working
		}
	}
	if len(upstream) == 0 {
		// REFUSE TO ACTIVATE. Leaving 127.0.0.1 set without a working
		// upstream would brick DNS system-wide — exactly what bit us
		// before. Surface a clear error and leave system DNS alone.
		// If we were ALREADY in the 127.0.0.1 state from a prior bad
		// run, recover by restoring user's DNS so DNS keeps working.
		if wasActive {
			log.Printf("em-walld: stuck in 127.0.0.1 with no working upstream — auto-restoring system DNS")
			_ = d.deactivateSystemDNS(ctx)
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
//
// Returns nil if nothing validates. Caller MUST decide whether to use
// a public fallback or surface an error.
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
	snap, err := d.sysDNS.CaptureAll()
	if err != nil {
		return false, fmt.Errorf("capture: %w", err)
	}
	clean := sanitizeSnapshot(snap)
	upstream := d.chooseUpstream(ctx, clean)
	if len(upstream) == 0 {
		// Last-ditch public fallback — still validated.
		if working := ValidateResolvers(ctx, []string{"1.1.1.1:53", "8.8.8.8:53"}); len(working) > 0 {
			upstream = working
		}
	}
	if len(upstream) == 0 {
		return false, fmt.Errorf("no working upstream DNS on current network")
	}
	if joinCSV(upstream) == joinCSV(current) {
		// New pick happens to match what we already had (e.g. validation
		// flapped). Don't churn the forwarder.
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

func (d *handlerDeps) deactivateSystemDNS(ctx context.Context) error {
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
	_ = d.store.SetSetting(ctx, "system_dns_active", "false")
	return nil
}

// normalizeGroupPattern lowercases and trims a pattern so comparisons
// don't depend on user-typed whitespace or trailing dots.
func normalizeGroupPattern(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(s, ".")))
}

// ruleCoveredByGroupPattern reports whether rulePat sits inside the
// scope of groupPat. Mirrors core/rules.Match's wildcard semantics so
// the same rules that *would* be hit by a group's wildcard at query
// time are also the ones treated as group members for delete-all /
// enable-all bulk actions.
//
//   group "*.openai.com" covers rules: openai.com, *.openai.com,
//                                       api.openai.com, *.api.openai.com
//   group "openai.com"   covers only:  openai.com (exact match)
//
// This is what the user expects: "chatgpt.com" should be considered
// part of the OpenAI group because OpenAI lists "*.chatgpt.com".
func ruleCoveredByGroupPattern(rulePat, groupPat string) bool {
	rp := normalizeGroupPattern(rulePat)
	gp := normalizeGroupPattern(groupPat)
	if rp == "" || gp == "" {
		return false
	}
	if rp == gp {
		return true
	}
	if !strings.HasPrefix(gp, "*.") {
		return false // exact-only group pattern
	}
	suffix := gp[2:]
	body := rp
	if strings.HasPrefix(rp, "*.") {
		body = rp[2:]
	}
	return body == suffix || strings.HasSuffix(body, "."+suffix)
}

// ruleBelongsToGroup is the union over a group's pattern list.
func ruleBelongsToGroup(rulePat string, groupPats []string) bool {
	for _, gp := range groupPats {
		if ruleCoveredByGroupPattern(rulePat, gp) {
			return true
		}
	}
	return false
}

// matchingRuleIDs returns IDs of rules covered by any of patterns. Used
// by the bulk delete handler. Order is unspecified.
func (d *handlerDeps) matchingRuleIDs(ctx context.Context, patterns []string) ([]int64, error) {
	all, err := d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range all {
		if ruleBelongsToGroup(r.Pattern, patterns) {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func ruleToDTO(r rules.Rule) ipc.RuleDTO {
	return ipc.RuleDTO{
		ID:        r.ID,
		Pattern:   r.Pattern,
		Action:    string(r.Action),
		Interface: r.Interface,
		Enabled:   r.Enabled,
		CreatedAt: r.CreatedAt.Format(time.RFC3339),
		UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
}

