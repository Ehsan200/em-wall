package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/portable"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/version"
	"github.com/ehsan/em-wall/core/xray"
)

// registerCustomGroupHandlers wires the CRUD for user-created groups. These
// manage the group *definition* only; rules created from a group's patterns
// are added/removed via the existing groups.apply / groups.delete-rules
// handlers, which already resolve custom keys through resolveGroupPatterns.
func registerCustomGroupHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodGroupsAdd, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsAddParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		g, err := d.store.AddCustomGroup(ctx, rules.CustomGroup{
			Key:         p.Key,
			DisplayName: p.DisplayName,
			Description: p.Description,
			Patterns:    p.Patterns,
			Color:       p.Color,
		})
		if err != nil {
			return nil, err
		}
		return customGroupToDTO(g), nil
	})

	s.Handle(ipc.MethodGroupsUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Key) == "" {
			return nil, rules.ErrEmptyGroupKey
		}
		if err := d.store.UpdateCustomGroup(ctx, rules.CustomGroup{
			Key:         p.Key,
			DisplayName: p.DisplayName,
			Description: p.Description,
			Patterns:    p.Patterns,
			Color:       p.Color,
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodGroupsDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.GroupsDeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		var result ipc.GroupsBulkResult
		// Optionally purge rules created from this group first.
		if p.DeleteRules {
			pats, err := d.resolveGroupPatterns(ctx, p.Key)
			if err != nil {
				return nil, err
			}
			ids, err := d.matchingRuleIDs(ctx, pats)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				_ = d.router.RemoveByRule(ctx, id)
				d.proxyTable.RemoveByRule(id)
				if err := d.store.Delete(ctx, id); err != nil {
					continue
				}
				result.RuleIDs = append(result.RuleIDs, id)
			}
			result.Affected = len(result.RuleIDs)
			_ = d.engine.Reload(ctx)
		}
		if err := d.store.DeleteCustomGroup(ctx, p.Key); err != nil {
			return nil, err
		}
		return result, nil
	})
}

// registerPortableHandlers wires encrypted export/import of rules + custom
// groups. The daemon owns the data and the crypto; the app handles file IO
// (save/open dialogs) and passes the base64 blob across.
func registerPortableHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodExport, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.ExportParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.Passphrase == "" {
			return nil, portable.ErrEmptyPassphrase
		}
		bundle, err := d.buildBundle(ctx, p.Selection)
		if err != nil {
			return nil, err
		}
		sealed, err := portable.Seal(bundle, p.Passphrase)
		if err != nil {
			return nil, err
		}
		return ipc.ExportResult{
			Blob:        base64.StdEncoding.EncodeToString(sealed),
			Filename:    fmt.Sprintf("em-wall-export-%s.embackup", time.Now().UTC().Format("20060102-150405")),
			RuleCount:   len(bundle.Rules),
			GroupCount:  len(bundle.CustomGroups),
			SubCount:    len(bundle.Subscriptions),
			MasterCount: len(bundle.Masters),
		}, nil
	})

	s.Handle(ipc.MethodImport, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.ImportParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(p.Blob)
		if err != nil {
			return nil, portable.ErrBadEnvelope
		}
		bundle, err := portable.Open(data, p.Passphrase)
		if err != nil {
			return nil, err
		}
		return d.applyBundle(ctx, bundle)
	})
}

// buildBundle assembles the plaintext export payload from a selection.
func (d *handlerDeps) buildBundle(ctx context.Context, sel ipc.ExportSelection) (portable.Bundle, error) {
	bundle := portable.Bundle{
		Schema:     portable.BundleSchema,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		AppVersion: version.Version,
	}

	// Collect rules, deduped by pattern (the unique key in the store).
	seenRule := make(map[string]bool)
	addRule := func(r rules.Rule) {
		if seenRule[r.Pattern] {
			return
		}
		seenRule[r.Pattern] = true
		bundle.Rules = append(bundle.Rules, portable.BundleRule{
			Pattern:   r.Pattern,
			Action:    string(r.Action),
			Interface: r.Interface,
			Enabled:   r.Enabled,
		})
	}

	if sel.All {
		all, err := d.store.List(ctx)
		if err != nil {
			return portable.Bundle{}, err
		}
		for _, r := range all {
			addRule(r)
		}
		cgs, err := d.store.ListCustomGroups(ctx)
		if err != nil {
			return portable.Bundle{}, err
		}
		for _, g := range cgs {
			bundle.CustomGroups = append(bundle.CustomGroups, customGroupToBundle(g))
		}
		// Subscriptions (config only — nodes re-fetched on import) and master
		// entries (xray entries with a Dialer) travel with a full export.
		subs, err := d.xrayStore.ListSubs(ctx)
		if err != nil {
			return portable.Bundle{}, err
		}
		for _, sub := range subs {
			bundle.Subscriptions = append(bundle.Subscriptions, portable.BundleSubscription{
				Name: sub.Name, URL: sub.URL, UserAgent: sub.UserAgent,
				IntervalSec: sub.IntervalSec, NodeCap: sub.NodeCap, Enabled: sub.Enabled,
			})
		}
		xs, err := d.xrayStore.List(ctx)
		if err != nil {
			return portable.Bundle{}, err
		}
		for _, e := range xs {
			if strings.TrimSpace(e.Dialer) == "" {
				continue
			}
			bundle.Masters = append(bundle.Masters, portable.BundleXray{
				Name: e.Name, Outbound: e.Outbound, Enabled: e.Enabled, Dialer: e.Dialer,
			})
		}
		return bundle, nil
	}

	// Explicit rule IDs.
	for _, id := range sel.RuleIDs {
		r, err := d.store.Get(ctx, id)
		if err != nil {
			if errors.Is(err, rules.ErrNotFound) {
				continue
			}
			return portable.Bundle{}, err
		}
		addRule(r)
	}

	// Curated groups → their matching rules (definition is code-side, not
	// exported; only the rules the user actually created travel).
	for _, key := range sel.CuratedKeys {
		g := groups.FindByKey(key)
		if g == nil {
			continue
		}
		if err := d.addRulesMatching(ctx, g.Patterns, addRule); err != nil {
			return portable.Bundle{}, err
		}
	}

	// Custom groups → definition + matching rules.
	for _, key := range sel.CustomKeys {
		g, err := d.store.GetCustomGroup(ctx, key)
		if err != nil {
			if errors.Is(err, rules.ErrGroupNotFound) {
				continue
			}
			return portable.Bundle{}, err
		}
		bundle.CustomGroups = append(bundle.CustomGroups, customGroupToBundle(g))
		if err := d.addRulesMatching(ctx, g.Patterns, addRule); err != nil {
			return portable.Bundle{}, err
		}
	}

	return bundle, nil
}

// addRulesMatching feeds every stored rule covered by patterns to add.
func (d *handlerDeps) addRulesMatching(ctx context.Context, patterns []string, add func(rules.Rule)) error {
	all, err := d.store.List(ctx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if ruleBelongsToGroup(r.Pattern, patterns) {
			add(r)
		}
	}
	return nil
}

// applyBundle imports a decrypted bundle, skipping existing custom groups
// (by key) and existing rules (by pattern). Reloads the engine and
// reconciles IP routes once at the end.
func (d *handlerDeps) applyBundle(ctx context.Context, b portable.Bundle) (ipc.ImportResult, error) {
	var res ipc.ImportResult

	// Custom groups first so a freshly imported group exists before any
	// follow-up apply the user might do.
	for _, g := range b.CustomGroups {
		_, err := d.store.AddCustomGroup(ctx, rules.CustomGroup{
			Key:         g.Key,
			DisplayName: g.DisplayName,
			Description: g.Description,
			Patterns:    g.Patterns,
			Color:       g.Color,
		})
		switch {
		case err == nil:
			res.GroupsCreated++
		case errors.Is(err, rules.ErrGroupDuplicate):
			res.GroupsSkipped++
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("group %q: %v", g.DisplayName, err))
		}
	}

	for _, r := range b.Rules {
		_, err := d.store.Add(ctx, rules.Rule{
			Pattern:   r.Pattern,
			Action:    rules.Action(r.Action),
			Interface: r.Interface,
			Enabled:   r.Enabled,
		})
		switch {
		case err == nil:
			res.RulesCreated++
		case errors.Is(err, rules.ErrDuplicate):
			res.RulesSkipped++
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("rule %q: %v", r.Pattern, err))
		}
	}

	// Subscriptions (before masters so an xraysub: dialer ref resolves).
	importedSub := false
	for _, sub := range b.Subscriptions {
		_, err := d.xrayStore.AddSub(ctx, xray.Subscription{
			Name: sub.Name, URL: sub.URL, UserAgent: sub.UserAgent,
			IntervalSec: sub.IntervalSec, NodeCap: sub.NodeCap, Enabled: sub.Enabled,
		})
		switch {
		case err == nil:
			res.SubsCreated++
			importedSub = true
		case errors.Is(err, xray.ErrDuplicate):
			res.SubsSkipped++
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("subscription %q: %v", sub.Name, err))
		}
	}

	// Master xray entries.
	importedMaster := false
	for _, m := range b.Masters {
		_, err := d.xrayStore.Add(ctx, xray.Config{
			Name: m.Name, Outbound: m.Outbound, Enabled: m.Enabled, Dialer: m.Dialer,
		})
		switch {
		case err == nil:
			res.MastersCreated++
			importedMaster = true
		case errors.Is(err, xray.ErrDuplicate):
			res.MastersSkipped++
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("master %q: %v", m.Name, err))
		}
	}

	if err := d.engine.Reload(ctx); err != nil {
		return res, fmt.Errorf("import applied, but engine reload failed: %w", err)
	}
	d.reconcileIPRoutes(ctx)

	// Bake any imported master slots and pull fresh nodes for imported subs.
	if importedMaster {
		if err := d.xraySup.Reconcile(ctx); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("xray reconcile after import: %v", err))
		}
	}
	if importedSub {
		go d.subFetch.RefreshAll(d.bgContext())
	}
	return res, nil
}

func customGroupToDTO(g rules.CustomGroup) ipc.GroupDTO {
	return ipc.GroupDTO{
		Key:         g.Key,
		DisplayName: g.DisplayName,
		Description: g.Description,
		Patterns:    g.Patterns,
		Color:       g.Color,
		Custom:      true,
	}
}

func customGroupToBundle(g rules.CustomGroup) portable.BundleGroup {
	return portable.BundleGroup{
		Key:         g.Key,
		DisplayName: g.DisplayName,
		Description: g.Description,
		Patterns:    g.Patterns,
		Color:       g.Color,
	}
}
