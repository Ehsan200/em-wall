package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// Promoting a subscription node into a standalone entry.
//
// A subscription's nodes are a pool: they exist to be ranked and dialed
// through a master entry's Dialer, and no rule can name one. Promoting
// copies one node's outbound into an ordinary xray.Config, which a rule
// can target as "xray:NAME" and an outbound set can list as a member.
//
// The copy is deliberate and is the whole design constraint. SubNode rows
// are volatile — ReplaceNodes wipes and rebuilds a subscription's pool on
// every fetch — so an entry that merely referenced one would break the
// next time the provider reordered its list. What the entry keeps instead
// is the origin (SubID + fingerprint), which is enough to mark the node as
// already promoted in the panel and to flag the entry once its source node
// stops appearing in the pool. That contrast is the same one curated
// groups draw against outbound sets: copies for things that must survive
// their source, references for things that must track it.

// xrayNameDedupeLimit bounds the "-2", "-3", … search for a free name.
// Reaching it means the user has a hundred entries whose names collapse to
// the same slug, at which point failing is better than counting forever.
const xrayNameDedupeLimit = 100

// uniqueXrayName resolves the name a promoted entry should take. An
// explicit name is honoured as given (and its collision reported, so the
// user sees why their choice was refused rather than silently getting a
// different one). A blank name is derived from the node's display name and
// deduped, because provider node names collide constantly once stripped to
// the entry charset — "🇩🇪 Frankfurt |2x" and "🇩🇪 Frankfurt |4x" both
// suggest "frankfurt-2x"-ish slugs and every node in a pool may share a
// prefix.
func (d *handlerDeps) uniqueXrayName(ctx context.Context, explicit, nodeName string) (string, error) {
	if explicit != "" {
		if !xray.ValidName(explicit) {
			return "", xray.ErrInvalidName
		}
		return explicit, nil
	}

	base := xray.SuggestName(nodeName)
	taken, err := d.takenXrayNames(ctx)
	if err != nil {
		return "", err
	}
	if !taken[base] {
		return base, nil
	}
	for i := 2; i <= xrayNameDedupeLimit; i++ {
		// Keep the suffix inside the 64-char name limit by trimming the
		// base, not by letting the candidate overflow and fail validation.
		suffix := "-" + strconv.Itoa(i)
		stem := base
		if len(stem)+len(suffix) > 64 {
			stem = stem[:64-len(suffix)]
		}
		candidate := stem + suffix
		if !taken[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for %q after %d attempts; name it explicitly", nodeName, xrayNameDedupeLimit)
}

// takenXrayNames is the set of entry names already in use, normalized the
// way the store's uniqueness index is.
func (d *handlerDeps) takenXrayNames(ctx context.Context) (map[string]bool, error) {
	list, err := d.xrayStore.List(ctx)
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool, len(list))
	for _, c := range list {
		taken[xray.SuggestName(c.Name)] = true
	}
	return taken, nil
}

// xrayOrigins loads what xrayToDTOWithOrigin needs: subscription names by
// ID, and the set of node keys currently live in any pool. Two queries for
// the whole entry list rather than two per entry.
func (d *handlerDeps) xrayOrigins(ctx context.Context) (map[int64]string, map[string]bool, error) {
	subs, err := d.xrayStore.ListSubs(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[int64]string, len(subs))
	for _, s := range subs {
		names[s.ID] = s.Name
	}
	live, err := d.xrayStore.LiveNodeKeys(ctx)
	if err != nil {
		return nil, nil, err
	}
	return names, live, nil
}

// xrayToDTOWithOrigin is xrayToDTO plus the promoted-entry provenance.
//
// SourceGone is reported only for an entry whose subscription still
// exists: once the subscription itself is deleted, an orphan entry's pool
// is gone by definition and saying "source node gone" would read as a
// problem with the entry rather than as the deletion the user just
// performed. Such an entry simply shows as hand-written, which is what it
// has effectively become.
func xrayToDTOWithOrigin(c xray.Config, subNames map[int64]string, live map[string]bool) ipc.XrayDTO {
	dto := xrayToDTO(c)
	if c.SubID == 0 || c.SubFingerprint == "" {
		return dto
	}
	subName, ok := subNames[c.SubID]
	if !ok {
		return dto
	}
	dto.SubName = subName
	dto.SourceGone = !live[xray.NodeKey(c.SubID, c.SubFingerprint)]
	return dto
}

// xrayDTO is the single-entry form, for handlers that return one row. A
// failure to load provenance degrades to the plain DTO rather than failing
// the call — the entry was stored either way, and provenance is a label.
func (d *handlerDeps) xrayDTO(ctx context.Context, c xray.Config) ipc.XrayDTO {
	subNames, live, err := d.xrayOrigins(ctx)
	if err != nil {
		return xrayToDTO(c)
	}
	return xrayToDTOWithOrigin(c, subNames, live)
}
