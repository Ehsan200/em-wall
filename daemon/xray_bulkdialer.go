package main

import (
	"context"
	"fmt"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// bulkEditDialer applies one Dialer edit to many entries. Validation runs
// against the whole proposed graph before anything is written — checking
// each entry against the pre-edit graph would miss a cycle the batch
// itself closes (a→b written alongside b→a) — and the write is a single
// transaction followed by a single Reconcile, so N entries cost one live
// apply instead of N.
func (d *handlerDeps) bulkEditDialer(ctx context.Context, p ipc.XrayBulkDialerParams) (ipc.XrayBulkDialerResult, error) {
	var res ipc.XrayBulkDialerResult
	if len(p.IDs) == 0 {
		return res, nil
	}
	refs, err := xray.ParseDialer(p.Dialer)
	if err != nil {
		return res, err
	}
	if len(refs) == 0 && p.Mode != xray.DialerEditReplace {
		return res, fmt.Errorf("%w: no dialer refs to %s", xray.ErrInvalidDialer, p.Mode)
	}
	if p.Mode != xray.DialerEditRemove {
		// Removing a ref that no longer exists is exactly how a user
		// cleans up a broken dialer, so only additions must resolve.
		if err := d.validateDialerRefs(ctx, refs); err != nil {
			return res, err
		}
	}

	all, err := d.xrayStore.List(ctx)
	if err != nil {
		return res, err
	}
	byID := make(map[int64]int, len(all))
	for i, c := range all {
		byID[c.ID] = i
	}
	changed := make(map[int64]string, len(p.IDs))
	for _, id := range p.IDs {
		i, ok := byID[id]
		if !ok {
			return res, fmt.Errorf("xray entry %d: %w", id, xray.ErrNotFound)
		}
		next, err := xray.EditDialer(all[i].Dialer, p.Mode, refs)
		if err != nil {
			return res, err
		}
		for _, r := range refs {
			if p.Mode != xray.DialerEditRemove && r.Kind == xray.DialerKindXray && r.Name == all[i].Name {
				return res, fmt.Errorf("%w: %s can't dial through itself — deselect it", xray.ErrDialerCycle, all[i].Name)
			}
		}
		if next != all[i].Dialer {
			changed[id] = next
		}
	}
	if len(changed) == 0 {
		return res, nil
	}

	// Cycle-check every changed entry against the graph with the whole
	// batch applied.
	proposed := make([]xray.Config, len(all))
	copy(proposed, all)
	for id, dl := range changed {
		proposed[byID[id]].Dialer = dl
	}
	for id, dl := range changed {
		c := proposed[byID[id]]
		if xray.DetectDialerCycle(proposed, id, c.Name, dl) {
			return res, fmt.Errorf("%w (via %s)", xray.ErrDialerCycle, c.Name)
		}
	}

	if err := d.xrayStore.SetDialers(ctx, changed); err != nil {
		return res, err
	}
	res.Updated = len(changed)
	if err := d.xraySup.Reconcile(ctx); err != nil {
		return res, fmt.Errorf("dialers updated, but xray reconcile failed: %w", err)
	}
	return res, nil
}
