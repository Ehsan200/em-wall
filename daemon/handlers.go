package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/applocator"
	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/routing"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/version"
	"github.com/ehsan/em-wall/core/xray"
)

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
		if err := d.validateProxyRefs(ctx, p.Interface); err != nil {
			return nil, err
		}
		if err := d.validateXrayRefs(ctx, p.Interface); err != nil {
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
		// Same reasoning for proxy mappings: drop any IP→proxy entries
		// this rule installed so a now-stale binding doesn't keep
		// dispatching cached IPs through the old proxy.
		d.proxyTable.RemoveByRule(p.ID)
		_ = d.engine.Reload(ctx)
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
				d.proxyTable.RemoveByRule(r.ID)
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
		// An IP-literal target dials by IP (no proxy-side DNS); a DNS
		// name is passed as the hostname so the proxy resolves it.
		var hostname string
		ip := net.ParseIP(d.proxyTestHost)
		if ip == nil {
			hostname = d.proxyTestHost
		}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		start := time.Now()
		conn, err := dialer.Dial(tctx, hostname, ip, d.proxyTestPort)
		if err != nil {
			return ipc.ProxiesTestResult{
				OK:      false,
				Message: fmt.Sprintf("connect via %s://%s:%d failed: %v", stored.Protocol, stored.Host, stored.Port, err),
			}, nil
		}
		// SOCKS5/HTTP CONNECT acceptance alone doesn't prove the chain
		// works: chained outbounds (VLESS+XHTTP etc.) accept the inbound
		// and only dial upstream on first payload. A TLS handshake forces
		// real bytes through the chain end-to-end.
		probeDeadline, _ := tctx.Deadline()
		if perr := proxy.ProbeTLS(conn, hostname, probeDeadline); perr != nil {
			return ipc.ProxiesTestResult{
				OK:      false,
				Message: fmt.Sprintf("TLS probe to %s via %s://%s:%d failed: %v", net.JoinHostPort(d.proxyTestHost, strconv.Itoa(d.proxyTestPort)), stored.Protocol, stored.Host, stored.Port, perr),
			}, nil
		}
		return ipc.ProxiesTestResult{
			OK:      true,
			Message: fmt.Sprintf("reached %s through proxy in %s", net.JoinHostPort(d.proxyTestHost, strconv.Itoa(d.proxyTestPort)), time.Since(start).Round(time.Millisecond)),
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
		added, err := d.xrayStore.Add(ctx, xray.Config{
			Name:     p.Name,
			Outbound: p.Outbound,
			Enabled:  p.Enabled,
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
		if err := d.xrayStore.Update(ctx, xray.Config{
			ID:       p.ID,
			Name:     p.Name,
			Outbound: p.Outbound,
			Enabled:  p.Enabled,
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

		var hostname string
		ip := net.ParseIP(host)
		if ip == nil {
			hostname = host
		}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		start := time.Now()
		conn, err := dialer.Dial(tctx, hostname, ip, port)
		if err != nil {
			return ipc.XrayTestResult{
				OK:      false,
				Message: fmt.Sprintf("connect to %s via xray:%s failed: %v", net.JoinHostPort(host, strconv.Itoa(port)), entry.Name, err),
			}, nil
		}
		// xray's SOCKS5 inbound accepts CONNECT before the user's
		// outbound (VLESS/VMess/etc.) actually dials upstream. A TLS
		// handshake forces a full round-trip through the outbound, so
		// "OK" here means the upstream xray server really is reachable
		// — not just that xray's inbound is listening.
		probeDeadline, _ := tctx.Deadline()
		if perr := proxy.ProbeTLS(conn, hostname, probeDeadline); perr != nil {
			return ipc.XrayTestResult{
				OK:      false,
				Message: fmt.Sprintf("xray:%s — local inbound reached, but upstream EOF'd Client Hello (chain likely broken): %v", entry.Name, perr),
			}, nil
		}
		elapsed := time.Since(start)
		return ipc.XrayTestResult{
			OK:        true,
			Message:   fmt.Sprintf("reached %s via xray:%s in %s", net.JoinHostPort(host, strconv.Itoa(port)), entry.Name, elapsed.Round(time.Millisecond)),
			LatencyMS: int(elapsed.Milliseconds()),
		}, nil
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
}

// validateProxyRefs checks that every proxy name referenced by a
// rule's Interface field actually exists in the proxy store. Returns
// nil for non-proxy interfaces (utunN, app:KEY, xray:NAME, empty), or
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
