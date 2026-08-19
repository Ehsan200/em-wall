package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/netprobe"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/routing"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/version"
	"github.com/ehsan/em-wall/core/xray"
)

// reconcileIPRoutes installs a kernel route for every enabled IP/CIDR
// route rule so its destination traffic is pinned to the right egress.
// proxy:/xray: rules route to the daemon's utun (the netstack handler then
// dials the chosen upstream); a literal-interface rule routes straight to
// that interface. Idempotent — already-present routes are a no-op — so it's
// safe to call after every rule change. Routes for removed/retargeted rules
// are dropped by the RemoveByRule calls the CRUD handlers already make.
// Best-effort: a single route failure is logged, not fatal.
func (d *handlerDeps) reconcileIPRoutes(ctx context.Context) {
	if d.router == nil {
		return
	}
	rs, err := d.store.List(ctx)
	if err != nil {
		log.Printf("em-walld: reconcile IP routes: list rules: %v", err)
		return
	}
	// Rules bound to an outbound set carry an indirect interface; resolve
	// it here the same way decision.Engine does for the DNS path,
	// otherwise a set-bound IP/CIDR rule would try to route via the
	// literal string "xrayset:NAME".
	rs = d.expandRuleIfaces(ctx, rs)
	for _, r := range rs {
		if r.Action != rules.ActionRoute || !r.Enabled || !rules.IsIPRule(r.Pattern) {
			continue
		}
		iface := r.Interface
		if proxy.IsProxyInterface(iface) || xray.IsXrayInterface(iface) {
			if d.proxyTun == "" {
				continue // proxy routing disabled (utun didn't open)
			}
			iface = d.proxyTun
		}
		if iface == "" {
			continue
		}
		if err := d.router.InstallStatic(ctx, r.Pattern, iface, r.ID); err != nil {
			log.Printf("em-walld: install IP route %s via %s failed: %v", r.Pattern, iface, err)
		}
	}
}

func registerHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodStatus, func(ctx context.Context, _ json.RawMessage) (any, error) {
		list, _ := d.store.List(ctx)
		blockEnc, _ := d.store.GetSetting(ctx, "block_encrypted_dns", "true")
		return ipc.StatusResult{
			Version:           version.Version,
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
		if err := d.validateProxyRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		if err := d.validateXrayRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		p.Interface = xray.CanonicalizeInterface(p.Interface)
		if err := d.validateSetRefs(ctx, p.Interface); err != nil {
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
		d.reconcileIPRoutes(ctx)
		return ruleToDTO(added), nil
	})

	s.Handle(ipc.MethodRulesUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RulesUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.validateProxyRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		if err := d.validateXrayRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		p.Interface = xray.CanonicalizeInterface(p.Interface)
		if err := d.validateSetRefs(ctx, p.Interface); err != nil {
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
		// this, switching a rule from utun4 to proxy:work would
		// leave the original utun4 routes in the OS table — letting
		// browser-cached IPs reach the destination via the wrong path.
		_ = d.router.RemoveByRule(ctx, p.ID)
		// Same reasoning for proxy mappings: drop any IP→proxy entries
		// this rule installed so a now-stale binding doesn't keep
		// dispatching cached IPs through the old proxy.
		d.proxyTable.RemoveByRule(p.ID)
		_ = d.engine.Reload(ctx)
		d.reconcileIPRoutes(ctx)
		return map[string]any{"ok": true}, nil
	})

	// Bulk retarget: change Action+Interface on every rule in IDs in a
	// single round-trip. Validates the new (Action, Interface) once, then
	// loops store.Update — same per-rule validation path as the single
	// update handler, so a malformed combination is rejected wholesale
	// before any row is touched. engine.Reload runs once at the end, not
	// per rule.
	s.Handle(ipc.MethodRulesBulkUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RulesBulkUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if len(p.IDs) == 0 {
			return nil, fmt.Errorf("no rule IDs given")
		}
		if p.Action == string(rules.ActionRoute) && strings.TrimSpace(p.Interface) == "" {
			return nil, fmt.Errorf("route action requires a non-empty interface")
		}
		if p.Action == string(rules.ActionBlock) && p.Interface != "" {
			// Block rules must have empty Interface — normalizeAction would
			// silently clear it, but be explicit so the UI can't accidentally
			// rely on an interface surviving a block flip.
			p.Interface = ""
		}
		if err := d.validateProxyRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		if err := d.validateXrayRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		p.Interface = xray.CanonicalizeInterface(p.Interface)
		if err := d.validateSetRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		var touched []int64
		for _, id := range p.IDs {
			r, err := d.store.Get(ctx, id)
			if err != nil {
				if errors.Is(err, rules.ErrNotFound) {
					continue // selection raced a delete; skip silently
				}
				return nil, err
			}
			r.Action = rules.Action(p.Action)
			r.Interface = p.Interface
			if err := d.store.Update(ctx, r); err != nil {
				return nil, err
			}
			// Flush per-host routes + proxy mappings tied to this rule so
			// cached IPs from the previous binding don't keep flowing the
			// old path. Same reasoning as MethodRulesUpdate above.
			_ = d.router.RemoveByRule(ctx, r.ID)
			d.proxyTable.RemoveByRule(r.ID)
			touched = append(touched, r.ID)
		}
		_ = d.engine.Reload(ctx)
		d.reconcileIPRoutes(ctx)
		return ipc.GroupsBulkResult{Affected: len(touched), RuleIDs: touched}, nil
	})

	s.Handle(ipc.MethodRulesDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RulesDeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		_ = d.router.RemoveByRule(ctx, p.ID)
		d.proxyTable.RemoveByRule(p.ID)
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
		// Validate BEFORE persisting so bad input never reaches the store.
		// Empty is allowed (clears back to the built-in default list).
		if p.Key == "fallback_dns" {
			if _, bad := parseDNSServers(p.Value); bad != "" {
				return nil, fmt.Errorf("invalid DNS server %q — use IPv4/IPv6 addresses (optionally host:port), separated by commas or newlines", bad)
			}
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
		// Side-effect: recompute the live upstream so an edited fallback
		// list takes effect immediately instead of only on the next
		// activate / network change. Best-effort — the value is already
		// saved and valid, so a transient re-pick failure (e.g. no network
		// right now) must not fail the save; the watcher will converge.
		if p.Key == "fallback_dns" {
			if _, err := d.repickUpstream(ctx); err != nil {
				log.Printf("em-walld: fallback_dns saved but upstream re-pick failed: %v", err)
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

	s.Handle(ipc.MethodLogsClear, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, d.store.ClearLogs(ctx)
	})

	s.Handle(ipc.MethodUsageQuery, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.UsageQueryParams
		_ = json.Unmarshal(raw, &p)
		pts, err := d.store.QueryTraffic(ctx, p.FromUnix, p.ToUnix, p.Dimension, p.BucketSecs)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.UsagePointDTO, len(pts))
		for i, pt := range pts {
			out[i] = ipc.UsagePointDTO{
				BucketUnix: pt.BucketTS,
				Key:        pt.Key,
				BytesSent:  pt.BytesSent,
				BytesRecv:  pt.BytesRecv,
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

	s.Handle(ipc.MethodGroupsList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		// Fetch rules once so each group can report how many stored rules
		// match its patterns (used by the UI to hide empty groups from the
		// export picker).
		allRules, err := d.store.List(ctx)
		if err != nil {
			return nil, err
		}
		countFor := func(patterns []string) int {
			n := 0
			for _, r := range allRules {
				if ruleBelongsToGroup(r.Pattern, patterns) {
					n++
				}
			}
			return n
		}

		registry := groups.KnownGroups()
		out := make([]ipc.GroupDTO, 0, len(registry))
		for _, g := range registry {
			pats := d.effectiveGroupPatterns(ctx, g)
			out = append(out, ipc.GroupDTO{
				Key:             g.Key,
				DisplayName:     g.DisplayName,
				Description:     g.Description,
				Patterns:        pats,
				Color:           groups.BrandColor(g.Key),
				Category:        groups.Category(g.Key),
				RuleCount:       countFor(pats),
				MissingPatterns: missingGroupPatterns(pats, allRules),
			})
		}
		// Append user-created groups after the curated registry.
		custom, err := d.store.ListCustomGroups(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range custom {
			out = append(out, ipc.GroupDTO{
				Key:             g.Key,
				DisplayName:     g.DisplayName,
				Description:     g.Description,
				Patterns:        g.Patterns,
				Color:           g.Color,
				Category:        groups.CategoryCustomKey,
				Custom:          true,
				RuleCount:       countFor(g.Patterns),
				MissingPatterns: missingGroupPatterns(g.Patterns, allRules),
			})
		}
		return out, nil
	})

	// Add-only top-up for a group that is already applied but has drifted
	// behind the shipped pattern list (see daemon/groups_sync.go).
	s.Handle(ipc.MethodGroupsSync, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsSyncParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return d.syncGroup(ctx, p)
	})

	s.Handle(ipc.MethodGroupsApply, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsApplyParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		pats, err := d.resolveGroupPatterns(ctx, p.Key)
		if err != nil {
			return nil, err
		}
		if len(pats) == 0 {
			return ipc.GroupsApplyResult{}, nil
		}
		// Validate the action / interface combination once up front so
		// we don't half-create the group on a bad request.
		probe := rules.Rule{
			Pattern: pats[0], Action: rules.Action(p.Action),
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
			out.Skipped = append(out.Skipped, pats[0])
		}
		for _, pattern := range pats[1:] {
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
		// A group may carry IP/CIDR patterns (e.g. Telegram's MTProto DC
		// ranges). Those become IP route rules that need a kernel static
		// route installed before traffic is pinned — Reload alone only
		// refreshes the in-memory rule cache. Idempotent, so harmless for
		// domain-only groups.
		d.reconcileIPRoutes(ctx)
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
		pats, err := d.resolveGroupPatterns(ctx, p.Key)
		if err != nil {
			return nil, err
		}
		ids, err := d.matchingRuleIDs(ctx, pats)
		if err != nil {
			return nil, err
		}
		var deleted []int64
		for _, id := range ids {
			_ = d.router.RemoveByRule(ctx, id)
			d.proxyTable.RemoveByRule(id)
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
		pats, err := d.resolveGroupPatterns(ctx, p.Key)
		if err != nil {
			return nil, err
		}
		all, err := d.store.List(ctx)
		if err != nil {
			return nil, err
		}
		var touched []int64
		for _, r := range all {
			if !ruleBelongsToGroup(r.Pattern, pats) {
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
				d.proxyTable.RemoveByRule(r.ID)
			}
			touched = append(touched, r.ID)
		}
		_ = d.engine.Reload(ctx)
		return ipc.GroupsBulkResult{Affected: len(touched), RuleIDs: touched}, nil
	})

	// Force a re-fetch of every dynamic group's vendor feed (Google's
	// published IP ranges today). The scheduler does this on its own
	// interval; this is the "refresh now" button's path.
	s.Handle(ipc.MethodGroupsRefresh, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return d.refreshAllDynamicGroups(ctx), nil
	})

	s.Handle(ipc.MethodGroupsIcon, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsIconParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		// Custom (user-created) group: synthesize an initials badge from
		// its display name + color; there's no shipped icon file.
		if strings.HasPrefix(p.Key, rules.CustomGroupPrefix) {
			cg, err := d.store.GetCustomGroup(ctx, p.Key)
			if err != nil {
				return nil, err
			}
			svg := groups.GenericSVG(cg.DisplayName, cg.Color)
			return ipc.GroupIconDTO{
				Key:     cg.Key,
				MIME:    "image/svg+xml",
				DataB64: base64.StdEncoding.EncodeToString([]byte(svg)),
			}, nil
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

	s.Handle(ipc.MethodProxiesList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		list, err := d.proxyStore.List(ctx)
		if err != nil {
			return nil, err
		}
		// Filter out the hidden rows the xray supervisor manages —
		// those are exposed to the UI through the Xray panel instead,
		// not the Proxies one.
		out := make([]ipc.ProxyDTO, 0, len(list))
		for _, p := range list {
			if xray.IsInternalProxyName(p.Name) {
				continue
			}
			out = append(out, proxyToDTO(p))
		}
		return out, nil
	})

	s.Handle(ipc.MethodProxiesAdd, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.ProxiesAddParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		// The "_xray_" prefix is reserved for supervisor-managed rows.
		// Refusing it here keeps the user from shadowing or colliding
		// with an entry the daemon will recreate on next Reconcile.
		if xray.IsInternalProxyName(strings.ToLower(strings.TrimSpace(p.Name))) {
			return nil, fmt.Errorf("proxy name %q is reserved", p.Name)
		}
		added, err := d.proxyStore.Add(ctx, proxy.Proxy{
			Name:     p.Name,
			Protocol: proxy.Protocol(p.Protocol),
			Host:     p.Host,
			Port:     p.Port,
			Username: p.Username,
			Password: p.Password,
		})
		if err != nil {
			return nil, err
		}
		return proxyToDTO(added), nil
	})

	s.Handle(ipc.MethodProxiesUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.ProxiesUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		// Snapshot the pre-update name so we can cascade-rename any
		// rule that references it via Interface = "proxy:OLD[,...]".
		// Skipped when the row doesn't exist — Update will surface
		// ErrNotFound with the proper error message.
		var oldName string
		if existing, gerr := d.proxyStore.Get(ctx, p.ID); gerr == nil {
			oldName = existing.Name
		}
		if err := d.proxyStore.Update(ctx, proxy.Proxy{
			ID:       p.ID,
			Name:     p.Name,
			Protocol: proxy.Protocol(p.Protocol),
			Host:     p.Host,
			Port:     p.Port,
			Username: p.Username,
			Password: p.Password,
		}); err != nil {
			return nil, err
		}
		if oldName != "" && !strings.EqualFold(oldName, p.Name) {
			if _, rerr := d.store.RenameInterfaceRef(ctx, proxy.InterfacePrefix, oldName, p.Name); rerr != nil {
				return nil, fmt.Errorf("proxy renamed, but cascading rule references failed: %w", rerr)
			}
			// Outbound sets reference proxies by name too.
			if _, serr := d.xrayStore.RenameSetMember(ctx, xray.DialerKindProxy, oldName, p.Name); serr != nil {
				return nil, fmt.Errorf("proxy renamed, but cascading set membership failed: %w", serr)
			}
			if err := d.engine.Reload(ctx); err != nil {
				return nil, fmt.Errorf("proxy renamed, but engine reload failed: %w", err)
			}
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodProxiesDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.ProxiesDeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		// Refuse to delete a proxy that's still referenced by any
		// rule's Interface field, otherwise that rule would silently
		// break at the next DNS query.
		stored, err := d.proxyStore.Get(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		refs, err := d.rulesReferencingProxy(ctx, stored.Name)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			return nil, fmt.Errorf("proxy %q is referenced by %d rule(s); remove or edit those rules first", stored.Name, len(refs))
		}
		if sets, serr := d.setMemberBlockers(ctx, xray.DialerKindProxy, stored.Name); serr != nil {
			return nil, serr
		} else if len(sets) > 0 {
			return nil, fmt.Errorf("proxy %q is a member of set(s): %s — remove it from those first", stored.Name, strings.Join(sets, ", "))
		}
		if err := d.proxyStore.Delete(ctx, p.ID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})

	// Live reachability check: open a connection through the proxy to a
	// well-known always-on TCP endpoint. Success means we reached the
	// proxy, completed its auth/handshake, and it connected onward. We
	// dial a raw IP (no hostname) so the probe doesn't depend on
	// proxy-side DNS — it's a pure reachability check of the proxy.
	s.Handle(ipc.MethodProxiesTest, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.ProxiesTestParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		stored, err := d.proxyStore.Get(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		dialer, err := proxy.NewDialer(stored)
		if err != nil {
			return ipc.ProxiesTestResult{OK: false, Message: err.Error()}, nil
		}
		// netprobe.Measure dials then forces a TLS handshake: SOCKS5/HTTP
		// CONNECT acceptance alone doesn't prove the chain works (chained
		// outbounds accept the inbound and only dial upstream on first
		// payload). The handshake pushes real bytes end-to-end.
		target := netprobe.Target{Host: d.proxyTestHost, Port: d.proxyTestPort}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r := netprobe.Measure(tctx, dialer, target)
		switch r.Stage {
		case netprobe.StageDial:
			return ipc.ProxiesTestResult{
				OK:      false,
				Message: fmt.Sprintf("connect via %s://%s:%d failed: %v", stored.Protocol, stored.Host, stored.Port, r.Err),
			}, nil
		case netprobe.StageTLS:
			return ipc.ProxiesTestResult{
				OK:      false,
				Message: fmt.Sprintf("TLS probe to %s via %s://%s:%d failed: %v", target, stored.Protocol, stored.Host, stored.Port, r.Err),
			}, nil
		}
		return ipc.ProxiesTestResult{
			OK:      true,
			Message: fmt.Sprintf("reached %s through proxy in %s", target, r.Latency.Round(time.Millisecond)),
		}, nil
	})

	s.Handle(ipc.MethodXrayStatus, func(_ context.Context, _ json.RawMessage) (any, error) {
		return ipc.XrayStatus{
			Enabled:        d.xraySup.Enabled(),
			Running:        d.xraySup.Running(),
			Version:        d.xraySup.Version(),
			PortRangeStart: xray.PortStart,
			PortRangeEnd:   xray.PortEnd,
			LastExit:       d.xraySup.LastExit(),
			RecentLogs:     d.xraySup.RecentLines(),
		}, nil
	})

	s.Handle(ipc.MethodXrayList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		list, err := d.xrayStore.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.XrayDTO, len(list))
		for i, c := range list {
			out[i] = xrayToDTO(c)
		}
		return out, nil
	})

	s.Handle(ipc.MethodXrayAdd, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XrayAddParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.validateDialer(ctx, 0, p.Name, p.Dialer); err != nil {
			return nil, err
		}
		added, err := d.xrayStore.Add(ctx, xray.Config{
			Name:     p.Name,
			Outbound: p.Outbound,
			Enabled:  p.Enabled,
			Dialer:   p.Dialer,
		})
		if err != nil {
			return nil, err
		}
		if err := d.xraySup.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("entry stored, but xray reconcile failed: %w", err)
		}
		return xrayToDTO(added), nil
	})

	s.Handle(ipc.MethodXrayUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XrayUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		// Capture the pre-rename name so we can cascade-update any rule
		// referencing it via "xray:OLD[,...]". A missing row is fine —
		// xrayStore.Update will return the canonical not-found error.
		var oldName string
		if existing, gerr := d.xrayStore.Get(ctx, p.ID); gerr == nil {
			oldName = existing.Name
		}
		if err := d.validateDialer(ctx, p.ID, p.Name, p.Dialer); err != nil {
			return nil, err
		}
		if err := d.xrayStore.Update(ctx, xray.Config{
			ID:       p.ID,
			Name:     p.Name,
			Outbound: p.Outbound,
			Enabled:  p.Enabled,
			Dialer:   p.Dialer,
		}); err != nil {
			return nil, err
		}
		if oldName != "" && !strings.EqualFold(oldName, p.Name) {
			if _, rerr := d.store.RenameInterfaceRef(ctx, xray.InterfacePrefix, oldName, p.Name); rerr != nil {
				return nil, fmt.Errorf("xray entry renamed, but cascading rule references failed: %w", rerr)
			}
			if err := d.engine.Reload(ctx); err != nil {
				return nil, fmt.Errorf("xray entry renamed, but engine reload failed: %w", err)
			}
			// Also cascade into other masters' Dialer "xray:OLD" refs, else
			// their dialer would point at a now-nonexistent entry.
			if _, derr := d.xrayStore.RenameDialerXrayRef(ctx, oldName, p.Name); derr != nil {
				return nil, fmt.Errorf("xray entry renamed, but cascading dialer references failed: %w", derr)
			}
			// ...and into outbound-set membership, which references
			// entries by name just as a dialer does.
			if _, serr := d.xrayStore.RenameSetMember(ctx, xray.DialerKindXray, oldName, p.Name); serr != nil {
				return nil, fmt.Errorf("xray entry renamed, but cascading set membership failed: %w", serr)
			}
			if err := d.engine.Reload(ctx); err != nil {
				return nil, fmt.Errorf("xray entry renamed, but engine reload failed: %w", err)
			}
		}
		if err := d.xraySup.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("entry updated, but xray reconcile failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXrayDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XrayDeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		// Block delete if any rule still references the entry — same
		// invariant as proxies, otherwise the rule silently breaks on
		// the next DNS query.
		stored, err := d.xrayStore.Get(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		refs, err := d.rulesReferencingXray(ctx, stored.Name)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			return nil, fmt.Errorf("xray entry %q is referenced by %d rule(s); remove those references first", stored.Name, len(refs))
		}
		// Also block if another master's Dialer references it, else that
		// dialer would silently lose a member.
		if dialers, derr := d.mastersReferencingXray(ctx, stored.Name); derr != nil {
			return nil, derr
		} else if len(dialers) > 0 {
			return nil, fmt.Errorf("xray entry %q is used by dialer(s): %s — clear those first", stored.Name, strings.Join(dialers, ", "))
		}
		// Same for outbound sets: a set that loses a member silently
		// shrinks the fallback chain of every rule bound to it.
		if sets, serr := d.setMemberBlockers(ctx, xray.DialerKindXray, stored.Name); serr != nil {
			return nil, serr
		} else if len(sets) > 0 {
			return nil, fmt.Errorf("xray entry %q is a member of set(s): %s — remove it from those first", stored.Name, strings.Join(sets, ", "))
		}
		if err := d.xrayStore.Delete(ctx, p.ID); err != nil {
			return nil, err
		}
		if err := d.xraySup.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("entry deleted, but xray reconcile failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXrayTest, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XrayTestParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		entry, err := d.xrayStore.Get(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		if !d.xraySup.Enabled() {
			return ipc.XrayTestResult{OK: false, Message: "xray binary not installed"}, nil
		}
		if !entry.Enabled {
			return ipc.XrayTestResult{OK: false, Message: "entry is disabled"}, nil
		}
		if !d.xraySup.Running() {
			msg := "xray subprocess is not running — see Recent xray output in the Xray panel"
			if le := d.xraySup.LastExit(); le != "" {
				msg += " (last exit: " + le + ")"
			}
			return ipc.XrayTestResult{OK: false, Message: msg}, nil
		}

		host, port, perr := parseTestTarget(p.Target, d.proxyTestHost, d.proxyTestPort)
		if perr != nil {
			return ipc.XrayTestResult{OK: false, Message: "invalid target: " + perr.Error()}, nil
		}

		// Dial through the entry's local SOCKS5 inbound. The supervisor
		// keeps a hidden proxy row for each entry, so we reuse the same
		// dialer code path proxies.test uses.
		dialer, err := proxy.NewDialer(proxy.Proxy{
			Protocol: proxy.ProtocolSOCKS5,
			Host:     "127.0.0.1",
			Port:     entry.SocksPort,
		})
		if err != nil {
			return ipc.XrayTestResult{OK: false, Message: err.Error()}, nil
		}

		// xray's SOCKS5 inbound accepts CONNECT before the user's outbound
		// (VLESS/VMess/etc.) actually dials upstream, so the TLS handshake
		// in Measure forces a full round-trip through the outbound: "OK"
		// means the upstream xray server is really reachable, not just that
		// the local inbound is listening.
		target := netprobe.Target{Host: host, Port: port}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r := netprobe.Measure(tctx, dialer, target)
		switch r.Stage {
		case netprobe.StageDial:
			return ipc.XrayTestResult{
				OK:      false,
				Message: fmt.Sprintf("connect to %s via xray:%s failed: %v", target, entry.Name, r.Err),
			}, nil
		case netprobe.StageTLS:
			return ipc.XrayTestResult{
				OK:      false,
				Message: fmt.Sprintf("xray:%s — local inbound reached, but upstream EOF'd Client Hello (chain likely broken): %v", entry.Name, r.Err),
			}, nil
		}
		elapsed := r.Latency

		// Exit identity comes from a live probe through the entry's own
		// SOCKS5 inbound and nothing else: ip-api.com sees the real public
		// exit IP after the whole chain. There is deliberately no fallback
		// to the configured server address — it is only the first hop, it
		// can be a CDN hostname, and resolving it locally can hand back one
		// of our own FakeIPs. Unknown is reported as empty, never guessed.
		exitIP, country, region, city, _ := probeExitIPVia(ctx, proxyDialContext(dialer))

		return ipc.XrayTestResult{
			OK:        true,
			Message:   fmt.Sprintf("reached %s via xray:%s in %s", net.JoinHostPort(host, strconv.Itoa(port)), entry.Name, elapsed.Round(time.Millisecond)),
			LatencyMS: int(elapsed.Milliseconds()),
			ExitIP:    exitIP,
			Country:   country,
			Region:    region,
			City:      city,
		}, nil
	})

	// net.public-ip — public egress identity for the system default
	// route ("what every site sees" for traffic no rule matches).
	s.Handle(ipc.MethodPublicIP, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return d.resolveExitIP(ctx, ""), nil
	})

	// rules.exit-ip — public egress identity for one rule's interface.
	// block rules short-circuit (no egress); allow rules fall through to
	// the default route via an empty interface.
	s.Handle(ipc.MethodRuleExitIP, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.RuleExitIPParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		r, err := d.store.Get(ctx, p.RuleID)
		if err != nil {
			return nil, err
		}
		if r.Action == rules.ActionBlock {
			return ipc.ExitIPResult{Interface: r.Interface, Message: "blocked — no egress"}, nil
		}
		return d.resolveExitIP(ctx, r.Interface), nil
	})

	s.Handle(ipc.MethodXrayParseLink, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XrayParseLinkParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		name, outbound, err := xray.ParseLink(p.Link)
		if err != nil {
			return nil, err
		}
		return ipc.XrayParseLinkResult{Name: name, Outbound: outbound}, nil
	})

	s.Handle(ipc.MethodXrayGetRouting, func(ctx context.Context, _ json.RawMessage) (any, error) {
		rules, err := d.xrayStore.GetRoutingRules(ctx)
		if err != nil {
			return nil, err
		}
		return ipc.XrayRoutingResult{Rules: rules}, nil
	})

	s.Handle(ipc.MethodXraySetRouting, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySetRoutingParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.xrayStore.SetRoutingRules(ctx, p.Rules); err != nil {
			return nil, err
		}
		if err := d.xraySup.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("rules saved, but xray reconcile failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXraySetEnabled, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySetEnabledParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		stored, err := d.xrayStore.Get(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		stored.Enabled = p.Enabled
		if err := d.xrayStore.Update(ctx, stored); err != nil {
			return nil, err
		}
		if err := d.xraySup.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("enabled flag updated, but xray reconcile failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	registerXraySubHandlers(s, d)
	registerXraySetHandlers(s, d)
	registerCustomGroupHandlers(s, d)
	registerPortableHandlers(s, d)
}

// registerXraySubHandlers wires the subscription CRUD + node ops and the
// observatory status method.
func registerXraySubHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodXraySubList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		subs, err := d.xrayStore.ListSubs(ctx)
		if err != nil {
			return nil, err
		}
		counts, err := d.xrayStore.AllNodeCounts(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.XraySubDTO, len(subs))
		for i, sub := range subs {
			c := counts[sub.ID]
			out[i] = subDTOFrom(sub, c[0], c[1])
		}
		return out, nil
	})

	s.Handle(ipc.MethodXraySubAdd, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubAddParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		sub, err := d.xrayStore.AddSub(ctx, xray.Subscription{
			Name: p.Name, URL: p.URL, UserAgent: p.UserAgent,
			IntervalSec: p.IntervalSec, NodeCap: p.NodeCap, Enabled: p.Enabled,
		})
		if err != nil {
			return nil, err
		}
		// Immediate background fetch so nodes appear without waiting for the
		// scheduler tick. Uses the daemon-lifetime context so it cancels on
		// shutdown rather than outliving it (bounded by fetch timeouts anyway).
		go func(id int64) {
			if _, ferr := d.subFetch.refreshOne(d.bgContext(), id); ferr != nil {
				d.subFetch.logger.Printf("subscription add %q: initial fetch failed: %v", p.Name, ferr)
			}
		}(sub.ID)
		total, active, err := d.xrayStore.CountNodes(ctx, sub.ID)
		if err != nil {
			return nil, err
		}
		return subDTOFrom(sub, total, active), nil
	})

	s.Handle(ipc.MethodXraySubUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		var oldName string
		if ex, gerr := d.xrayStore.GetSub(ctx, p.ID); gerr == nil {
			oldName = ex.Name
		}
		if err := d.xrayStore.UpdateSub(ctx, xray.Subscription{
			ID: p.ID, Name: p.Name, URL: p.URL, UserAgent: p.UserAgent,
			IntervalSec: p.IntervalSec, NodeCap: p.NodeCap, Enabled: p.Enabled,
		}); err != nil {
			return nil, err
		}
		if oldName != "" && !strings.EqualFold(oldName, p.Name) {
			if _, cerr := d.xrayStore.RenameDialerSubRef(ctx, oldName, p.Name); cerr != nil {
				return nil, fmt.Errorf("subscription renamed, but cascading dialer refs failed: %w", cerr)
			}
		}
		// Enable/cap/rename can shift active membership → live-sync (falls
		// back to a Reconcile on a structural change).
		if err := d.xraySup.SyncDialerMembers(ctx); err != nil {
			return nil, fmt.Errorf("subscription updated, but dialer sync failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXraySubDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubDeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		sub, err := d.xrayStore.GetSub(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		refs, err := d.mastersReferencingSub(ctx, sub.Name)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			return nil, fmt.Errorf("subscription %q is used by dialer(s): %s — clear those first", sub.Name, strings.Join(refs, ", "))
		}
		if err := d.xrayStore.DeleteSub(ctx, p.ID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXraySubSetEnabled, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubSetEnabledParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.xrayStore.SetSubEnabled(ctx, p.ID, p.Enabled); err != nil {
			return nil, err
		}
		if err := d.xraySup.SyncDialerMembers(ctx); err != nil {
			return nil, fmt.Errorf("subscription toggled, but dialer sync failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXraySubRefresh, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubRefreshParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.ID == 0 {
			d.subFetch.RefreshAll(ctx)
			return ipc.XraySubRefreshResult{OK: true}, nil
		}
		n, err := d.subFetch.refreshOne(ctx, p.ID)
		if err != nil {
			return ipc.XraySubRefreshResult{OK: false, Message: err.Error()}, nil
		}
		return ipc.XraySubRefreshResult{OK: true, Nodes: n}, nil
	})

	s.Handle(ipc.MethodXraySubNodes, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubNodesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		nodes, err := d.xrayStore.ListNodes(ctx, p.SubID)
		if err != nil {
			return nil, err
		}
		disabled, err := d.xrayStore.DisabledFingerprints(ctx, p.SubID)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.XraySubNodeDTO, len(nodes))
		for i, n := range nodes {
			out[i] = ipc.XraySubNodeDTO{
				Fingerprint: n.Fingerprint, Name: n.Name, Active: n.Active,
				Disabled: disabled[n.Fingerprint], LatencyMs: n.LastLatencyMs,
			}
		}
		return out, nil
	})

	s.Handle(ipc.MethodXraySubSetNodeDisabled, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySubSetNodeDisabledParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.xrayStore.SetNodeDisabled(ctx, p.SubID, p.Fingerprint, p.Disabled); err != nil {
			return nil, err
		}
		if err := d.xraySup.SyncDialerMembers(ctx); err != nil {
			return nil, fmt.Errorf("node toggled, but dialer sync failed: %w", err)
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXrayObservatory, func(ctx context.Context, _ json.RawMessage) (any, error) {
		raw, err := d.xraySup.BalancerInfoRaw(ctx)
		if err != nil {
			// Soft-fail: no balancers running / api not ready → empty.
			return ipc.XrayObservatoryResult{}, nil
		}
		return ipc.XrayObservatoryResult{
			Winners: xray.ParseBalancerWinners(string(raw)),
			Raw:     string(raw),
		}, nil
	})
}

// bgContext returns the daemon-lifetime context for detached background
// work (cancels on shutdown), falling back to Background when unset.
func (d *handlerDeps) bgContext() context.Context {
	if d.bgCtx != nil {
		return d.bgCtx
	}
	return context.Background()
}

// subDTOFrom builds a subscription DTO from a stored row and its counts.
func subDTOFrom(sub xray.Subscription, total, active int) ipc.XraySubDTO {
	last := ""
	if !sub.LastFetched.IsZero() {
		last = sub.LastFetched.Format(time.RFC3339)
	}
	return ipc.XraySubDTO{
		ID: sub.ID, Name: sub.Name, URL: sub.URL, UserAgent: sub.UserAgent,
		IntervalSec: sub.IntervalSec, NodeCap: sub.NodeCap, Enabled: sub.Enabled,
		LastFetched: last, LastError: sub.LastError,
		NodeCount: total, ActiveCount: active,
		UsageUpload:   sub.Upload,
		UsageDownload: sub.Download,
		UsageTotal:    sub.Total,
		UsageExpire:   sub.Expire,
		CreatedAt:     sub.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     sub.UpdatedAt.Format(time.RFC3339),
	}
}

// mastersReferencingSub / mastersReferencingXray return the names of
// master entries whose Dialer references the named subscription / xray
// entry — used to block a delete that would leave a dangling dialer ref.
func (d *handlerDeps) mastersReferencingSub(ctx context.Context, subName string) ([]string, error) {
	return d.mastersReferencingDialer(ctx, xray.DialerKindXraysub, subName)
}

func (d *handlerDeps) mastersReferencingXray(ctx context.Context, entryName string) ([]string, error) {
	return d.mastersReferencingDialer(ctx, xray.DialerKindXray, entryName)
}

func (d *handlerDeps) mastersReferencingDialer(ctx context.Context, kind, name string) ([]string, error) {
	all, err := d.xrayStore.List(ctx)
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	var out []string
	for _, c := range all {
		refs, perr := xray.ParseDialer(c.Dialer)
		if perr != nil {
			continue
		}
		for _, r := range refs {
			if r.Kind == kind && r.Name == name {
				out = append(out, c.Name)
				break
			}
		}
	}
	return out, nil
}

// validateDialer checks that a master's dialer refs all resolve and that
// the resulting master-dialer graph has no cycle involving this entry.
// selfID is 0 on add. Empty dialer is always valid.
func (d *handlerDeps) validateDialer(ctx context.Context, selfID int64, selfName, dialer string) error {
	refs, err := xray.ParseDialer(dialer)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	xNames, subNames, pNames := xray.RefsByKind(refs)
	if len(xNames) > 0 {
		missing, err := d.xrayStore.NamesExist(ctx, xNames)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("dialer references unknown xray entr(ies): %s", strings.Join(missing, ", "))
		}
	}
	if len(subNames) > 0 {
		missing, err := d.xrayStore.SubNamesExist(ctx, subNames)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("dialer references unknown subscription(s): %s", strings.Join(missing, ", "))
		}
	}
	if len(pNames) > 0 {
		missing, err := d.proxyStore.NamesExist(ctx, pNames)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("dialer references unknown proxy(ies): %s", strings.Join(missing, ", "))
		}
	}
	return d.checkDialerCycle(ctx, selfID, selfName, dialer)
}

// checkDialerCycle detects a cycle in the master-dialer graph formed by
// xray: refs (only entries can loop; sub/proxy refs are leaves). The graph
// logic lives in core/xray so it is unit-testable without daemon deps.
func (d *handlerDeps) checkDialerCycle(ctx context.Context, selfID int64, selfName, dialer string) error {
	all, err := d.xrayStore.List(ctx)
	if err != nil {
		return err
	}
	if xray.DetectDialerCycle(all, selfID, selfName, dialer) {
		return xray.ErrDialerCycle
	}
	return nil
}

// validateProxyRefs checks that every proxy name referenced by a
// rule's Interface field actually exists in the proxy store. Returns
// nil for non-proxy interfaces (utunN, xray:NAME, empty), or
// a wrapped error naming the missing proxies.
func (d *handlerDeps) validateProxyRefs(ctx context.Context, iface string) error {
	if !proxy.IsProxyInterface(iface) {
		return nil
	}
	names := proxy.ParseInterface(iface)
	if len(names) == 0 {
		return fmt.Errorf("interface %q references no proxy names", iface)
	}
	missing, err := d.proxyStore.NamesExist(ctx, names)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("unknown proxy reference(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateXrayRefs is the xray analogue of validateProxyRefs — a rule
// referencing xray:NAME must point at an entry that exists in the
// xray store. Returns nil for non-xray interfaces.
func (d *handlerDeps) validateXrayRefs(ctx context.Context, iface string) error {
	if !xray.IsXrayInterface(iface) {
		return nil
	}
	names := xray.ParseInterface(iface)
	if len(names) == 0 {
		return fmt.Errorf("interface %q references no xray entry names", iface)
	}
	missing, err := d.xrayStore.NamesExist(ctx, names)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("unknown xray entry reference(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// rulesReferencingProxy returns the IDs of rules whose Interface
// field includes name in a "proxy:NAME[,...]" list.
func (d *handlerDeps) rulesReferencingProxy(ctx context.Context, name string) ([]int64, error) {
	all, err := d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range all {
		if !proxy.IsProxyInterface(r.Interface) {
			continue
		}
		if slices.Contains(proxy.ParseInterface(r.Interface), name) {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// rulesReferencingXray returns the IDs of rules whose Interface field
// includes name in an "xray:NAME[,...]" list. Mirrors
// rulesReferencingProxy so delete-time validation is symmetric.
func (d *handlerDeps) rulesReferencingXray(ctx context.Context, name string) ([]int64, error) {
	all, err := d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range all {
		if !xray.IsXrayInterface(r.Interface) {
			continue
		}
		if slices.Contains(xray.ParseInterface(r.Interface), name) {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}
