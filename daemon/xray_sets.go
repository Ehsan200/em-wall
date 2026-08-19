package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// ---------- outbound sets ----------
//
// A set is a named, ordered bundle of outbounds ("xrayset:NAME" on a
// rule's Interface). decision.Engine resolves the indirection once per
// Reload, so the DNS and netstack paths never see it. Everything in this
// file is the COLD path: IPC CRUD plus the handful of daemon readers
// that inspect stored rule rows directly instead of going through the
// engine, and so have to expand for themselves.

// expandIface resolves an indirect set binding to its literal interface
// string, mirroring what decision.Engine.Reload does for the hot path.
// Non-set values and unresolvable sets come back unchanged — see
// xray.ExpandSetInterface for why a dangling ref is kept verbatim.
//
// Best-effort: a store error yields the input untouched, which fails
// closed for a set ref (nothing downstream matches it) and is a no-op
// for everything else.
func (d *handlerDeps) expandIface(ctx context.Context, iface string) string {
	if !xray.IsSetInterface(iface) || d.xrayStore == nil {
		return iface
	}
	exp, err := d.xrayStore.SetExpansions(ctx)
	if err != nil {
		return iface
	}
	return xray.ExpandSetInterface(iface, exp)
}

// expandRuleIfaces returns rs with every set-bound Interface resolved,
// for readers that iterate the whole rule list. One store round-trip
// regardless of rule count.
func (d *handlerDeps) expandRuleIfaces(ctx context.Context, rs []rules.Rule) []rules.Rule {
	if d.xrayStore == nil {
		return rs
	}
	need := false
	for i := range rs {
		if xray.IsSetInterface(rs[i].Interface) {
			need = true
			break
		}
	}
	if !need {
		return rs
	}
	exp, err := d.xrayStore.SetExpansions(ctx)
	if err != nil {
		return rs
	}
	out := make([]rules.Rule, len(rs))
	copy(out, rs)
	for i := range out {
		out[i].Interface = xray.ExpandSetInterface(out[i].Interface, exp)
	}
	return out
}

// validateSetRefs rejects a rule bound to a set that doesn't exist, the
// set-shaped sibling of validateProxyRefs / validateXrayRefs.
func (d *handlerDeps) validateSetRefs(ctx context.Context, iface string) error {
	name := xray.ParseSetName(iface)
	if name == "" {
		return nil
	}
	missing, err := d.xrayStore.SetNamesExist(ctx, []string{name})
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("unknown outbound set reference: %s", strings.Join(missing, ", "))
	}
	return nil
}

// rulesReferencingSet returns the IDs of rules bound to set name.
func (d *handlerDeps) rulesReferencingSet(ctx context.Context, name string) ([]int64, error) {
	all, err := d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range all {
		if xray.ParseSetName(r.Interface) == name {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// setMemberBlockers reports the sets that would be broken by deleting
// the given member, so entry/proxy deletion can refuse the same way it
// already refuses when a RULE still points at the target.
func (d *handlerDeps) setMemberBlockers(ctx context.Context, kind, name string) ([]string, error) {
	if d.xrayStore == nil {
		return nil, nil
	}
	return d.xrayStore.SetsReferencing(ctx, kind, name)
}

// setToDTO enriches a stored set with the counts the UI badges it by:
// which members have vanished, how many are actually usable right now,
// how many rules would feel an edit, and what the set expands to.
func (d *handlerDeps) setToDTO(ctx context.Context, s xray.Set) ipc.XraySetDTO {
	dto := ipc.XraySetDTO{
		ID:        s.ID,
		Name:      s.Name,
		Members:   []string{},
		Enabled:   s.Enabled,
		CreatedAt: s.CreatedAt.Format(time.RFC3339),
		UpdatedAt: s.UpdatedAt.Format(time.RFC3339),
	}
	refs, err := xray.ParseSetMembers(s.Members)
	if err != nil {
		// Unparseable row (hand-edited DB): surface it as a set with no
		// usable members rather than dropping it from the list, so the
		// user can see it and fix or delete it.
		return dto
	}
	dto.Members = make([]string, len(refs))
	for i, r := range refs {
		dto.Members[i] = r.Kind + ":" + r.Name
	}
	dto.Interface = xray.ExpandMembers(refs)

	for _, r := range refs {
		switch r.Kind {
		case xray.DialerKindXray:
			e, err := d.xrayStore.GetByName(ctx, r.Name)
			if err != nil {
				dto.MissingMembers = append(dto.MissingMembers, r.Kind+":"+r.Name)
				continue
			}
			if e.Enabled {
				dto.UsableCount++
			}
		case xray.DialerKindProxy:
			// A proxy row has no enabled flag — existing is usable.
			if _, err := d.proxyStore.GetByName(ctx, r.Name); err != nil {
				dto.MissingMembers = append(dto.MissingMembers, r.Kind+":"+r.Name)
				continue
			}
			dto.UsableCount++
		}
	}
	if ids, err := d.rulesReferencingSet(ctx, s.Name); err == nil {
		dto.RuleCount = len(ids)
	}
	return dto
}

// membersFromDTO joins the UI's member list back into the canonical
// stored form. Validation happens in the store.
func membersFromDTO(members []string) string {
	return strings.Join(members, ",")
}

func registerXraySetHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodXraySetsList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		list, err := d.xrayStore.ListSets(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ipc.XraySetDTO, 0, len(list))
		for _, st := range list {
			out = append(out, d.setToDTO(ctx, st))
		}
		return out, nil
	})

	s.Handle(ipc.MethodXraySetsAdd, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySetAddParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.validateSetMembers(ctx, p.Members); err != nil {
			return nil, err
		}
		added, err := d.xrayStore.AddSet(ctx, xray.Set{
			Name:    p.Name,
			Members: membersFromDTO(p.Members),
			Enabled: p.Enabled,
		})
		if err != nil {
			return nil, err
		}
		return d.setToDTO(ctx, added), nil
	})

	s.Handle(ipc.MethodXraySetsUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XraySetUpdateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if err := d.validateSetMembers(ctx, p.Members); err != nil {
			return nil, err
		}
		// Capture the pre-rename name so rules bound to the old one can
		// be cascaded, exactly as an xray entry rename does.
		var oldName string
		if existing, gerr := d.xrayStore.GetSet(ctx, p.ID); gerr == nil {
			oldName = existing.Name
		}
		if err := d.xrayStore.UpdateSet(ctx, xray.Set{
			ID:      p.ID,
			Name:    p.Name,
			Members: membersFromDTO(p.Members),
			Enabled: p.Enabled,
		}); err != nil {
			return nil, err
		}
		if oldName != "" && !strings.EqualFold(oldName, p.Name) {
			// Cascade using the CANONICAL stored name: UpdateSet lowercases
			// the row, and rule refs are matched against the expansion map
			// by exact string, so writing the user's raw casing here would
			// leave the rules pointing at a key that never resolves.
			renamed, gerr := d.xrayStore.GetSet(ctx, p.ID)
			if gerr != nil {
				return nil, fmt.Errorf("set updated, but re-reading it failed: %w", gerr)
			}
			if _, rerr := d.store.RenameInterfaceRef(ctx, xray.SetPrefix, oldName, renamed.Name); rerr != nil {
				return nil, fmt.Errorf("set renamed, but cascading rule references failed: %w", rerr)
			}
		}
		// Members may have changed under rules that bind this set — that
		// IS the point of a set — so the engine must re-expand, and any
		// IP/CIDR rule bound to it may need its static route retargeted.
		if err := d.engine.Reload(ctx); err != nil {
			return nil, fmt.Errorf("set updated, but engine reload failed: %w", err)
		}
		d.reconcileIPRoutes(ctx)
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXraySetsSetEnabled, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ID      int64 `json:"id"`
			Enabled bool  `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		st, err := d.xrayStore.GetSet(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		st.Enabled = p.Enabled
		if err := d.xrayStore.UpdateSet(ctx, st); err != nil {
			return nil, err
		}
		// Disabling drops the set out of the expansion map, so its rules
		// stop resolving (fail closed) from the next query on.
		if err := d.engine.Reload(ctx); err != nil {
			return nil, fmt.Errorf("set toggled, but engine reload failed: %w", err)
		}
		d.reconcileIPRoutes(ctx)
		return map[string]any{"ok": true}, nil
	})

	s.Handle(ipc.MethodXraySetsDelete, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		st, err := d.xrayStore.GetSet(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		// Same invariant as deleting a proxy or an xray entry: a rule
		// left pointing at a deleted set would silently stop resolving.
		refs, err := d.rulesReferencingSet(ctx, st.Name)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			return nil, fmt.Errorf("set %q is referenced by %d rule(s); remove or rebind those rules first", st.Name, len(refs))
		}
		if err := d.xrayStore.DeleteSet(ctx, p.ID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// validateSetMembers checks that every proposed member still exists, so
// a set can't be created pointing at nothing. Parse errors surface from
// the store; this only covers existence.
func (d *handlerDeps) validateSetMembers(ctx context.Context, members []string) error {
	refs, err := xray.ParseSetMembers(membersFromDTO(members))
	if err != nil {
		return err
	}
	var xrayNames, proxyNames []string
	for _, r := range refs {
		switch r.Kind {
		case xray.DialerKindXray:
			xrayNames = append(xrayNames, r.Name)
		case xray.DialerKindProxy:
			proxyNames = append(proxyNames, r.Name)
		}
	}
	if len(xrayNames) > 0 {
		missing, err := d.xrayStore.NamesExist(ctx, xrayNames)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("unknown xray entry member(s): %s", strings.Join(missing, ", "))
		}
	}
	if len(proxyNames) > 0 {
		missing, err := d.proxyStore.NamesExist(ctx, proxyNames)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("unknown proxy member(s): %s", strings.Join(missing, ", "))
		}
	}
	return nil
}
