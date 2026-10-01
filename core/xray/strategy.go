package xray

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// Pool switch strategies (Subscription.Strategy, DialerSlot.Strategy).
//
// Stable suits pools whose nodes stay up or down for long stretches: the
// daemon ranks members over time (smoothed shortlist), keeps a sticky pair,
// and parks nodes that stay dead. Agile suits pools whose nodes come and go
// in waves a minute or so apart: no parking, no smoothing — the balancer
// spreads over the members alive right now. Manual routes only through the
// nodes the user pinned: no switching away from them, no parking. Auto
// (the default) lets the daemon pick stable or agile per pool from its
// health history. Ported from em-xray, where it is in production.
const (
	StrategyAuto   = "auto"
	StrategyStable = "stable"
	StrategyAgile  = "agile"
	StrategyManual = "manual"
)

// ParseStrategy normalizes a user-given strategy ("" means auto).
func ParseStrategy(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", StrategyAuto:
		return StrategyAuto, nil
	case StrategyStable, StrategyAgile, StrategyManual:
		return v, nil
	}
	return "", fmt.Errorf("unknown strategy %q (want auto, stable, agile or manual)", s)
}

// EffectiveStrategy returns the subscription's strategy, auto when unset.
func (s Subscription) EffectiveStrategy() string {
	if v, err := ParseStrategy(s.Strategy); err == nil {
		return v
	}
	return StrategyAuto
}

// SetSubStrategy stores a subscription's switch strategy.
func (s *Store) SetSubStrategy(ctx context.Context, id int64, strategy string) error {
	v, err := ParseStrategy(strategy)
	if err != nil {
		return err
	}
	res := s.db.WithContext(ctx).Model(&Subscription{}).Where("id = ?", id).Update("strategy", v)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodePinned pins or unpins one node of a subscription. A pinned node
// is always active (outside the node cap) and pinning re-enables a node
// the user had disabled.
func (s *Store) SetNodePinned(ctx context.Context, subID int64, fingerprint string, pinned bool) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ErrNotFound
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ov SubNodeOverride
		err := tx.First(&ov, "sub_id = ? AND fingerprint = ?", subID, fingerprint).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			if !pinned {
				return nil
			}
			return tx.Create(&SubNodeOverride{SubID: subID, Fingerprint: fingerprint, Pinned: true}).Error
		case err != nil:
			return err
		case !pinned && !ov.Disabled:
			return tx.Delete(&SubNodeOverride{}, ov.ID).Error
		case pinned:
			return tx.Model(&SubNodeOverride{}).Where("id = ?", ov.ID).
				Updates(map[string]any{"pinned": true, "disabled": false}).Error
		default:
			return tx.Model(&SubNodeOverride{}).Where("id = ?", ov.ID).Update("pinned", false).Error
		}
	})
	if err != nil {
		return fmt.Errorf("set node pinned: %w", err)
	}
	return s.recomputeActive(ctx, subID)
}

// ClearPins unpins every node of a subscription.
func (s *Store) ClearPins(ctx context.Context, subID int64) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("sub_id = ? AND pinned = ? AND disabled = ?", subID, true, false).
			Delete(&SubNodeOverride{}).Error; err != nil {
			return err
		}
		return tx.Model(&SubNodeOverride{}).Where("sub_id = ?", subID).Update("pinned", false).Error
	})
	if err != nil {
		return fmt.Errorf("clear pins: %w", err)
	}
	return s.recomputeActive(ctx, subID)
}

// PinnedFingerprints returns the set of fingerprints pinned for a
// subscription.
func (s *Store) PinnedFingerprints(ctx context.Context, subID int64) (map[string]bool, error) {
	var ov []SubNodeOverride
	if err := s.db.WithContext(ctx).
		Where("sub_id = ? AND pinned = ?", subID, true).Find(&ov).Error; err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(ov))
	for _, o := range ov {
		m[o.Fingerprint] = true
	}
	return m, nil
}
