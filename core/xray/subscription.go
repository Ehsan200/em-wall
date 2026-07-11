package xray

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Defaults applied when a Subscription is stored with the corresponding
// field left zero. IntervalSec / NodeCap fall back to these at fetch and
// activation time so a caller can create a subscription with just a name
// and URL.
const (
	// DefaultSubUserAgent is sent when a subscription has no UserAgent.
	// Many providers gate the returned node list on a v2rayN-ish UA.
	DefaultSubUserAgent = "v2rayN/6.23"
	// DefaultSubIntervalSec is the auto-refresh cadence (12h).
	DefaultSubIntervalSec = 12 * 3600
	// DefaultSubNodeCap bounds how many nodes are activated (and thus
	// pinged by observatory) per subscription.
	DefaultSubNodeCap = 30
)

// Subscription is a remote URL that yields a base64 list of share links.
// The daemon fetches it on a schedule and materializes the parsed nodes
// into SubNode rows. A subscription is never a rule target itself — its
// nodes are consumed only through a master entry's Dialer field.
type Subscription struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	Name        string    `gorm:"not null;uniqueIndex;column:name"`
	URL         string    `gorm:"not null;column:url;type:text"`
	UserAgent   string    `gorm:"column:user_agent;type:text;not null;default:''"`
	IntervalSec int       `gorm:"not null;default:0;column:interval_sec"`
	NodeCap     int       `gorm:"not null;default:0;column:node_cap"`
	Enabled     bool      `gorm:"not null;default:true;column:enabled"`
	LastFetched time.Time `gorm:"column:last_fetched"`
	LastError   string    `gorm:"column:last_error;type:text;not null;default:''"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Subscription) TableName() string { return "xray_subscriptions" }

// SubNode is one parsed node in a subscription's pool. Rows are volatile:
// each fetch replaces the whole set for a subscription (see ReplaceNodes).
// Fingerprint is the stable identity used to re-apply overrides across a
// refresh; Active is derived (subscription enabled, not overridden-off,
// within the node cap) and read by config generation.
type SubNode struct {
	ID            int64     `gorm:"primaryKey;column:id"`
	SubID         int64     `gorm:"not null;index;column:sub_id"`
	Name          string    `gorm:"not null;column:name"`
	Fingerprint   string    `gorm:"not null;index;column:fingerprint"`
	Outbound      string    `gorm:"not null;column:outbound;type:text"`
	Active        bool      `gorm:"not null;default:false;column:active"`
	LastLatencyMs int       `gorm:"not null;default:-1;column:last_latency_ms"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (SubNode) TableName() string { return "xray_sub_nodes" }

// SubNodeOverride durably records a per-node manual disable. It outlives
// the volatile SubNode rows so toggling a node off survives a refresh,
// daemon restart, or the node changing position in the list. Keyed by
// (SubID, Fingerprint).
type SubNodeOverride struct {
	ID          int64  `gorm:"primaryKey;column:id"`
	SubID       int64  `gorm:"not null;uniqueIndex:idx_sub_fp;column:sub_id"`
	Fingerprint string `gorm:"not null;uniqueIndex:idx_sub_fp;column:fingerprint"`
	Disabled    bool   `gorm:"not null;default:false;column:disabled"`
}

func (SubNodeOverride) TableName() string { return "xray_sub_node_overrides" }

// Fingerprint is a stable identity hash of an outbound JSON block,
// independent of its "tag" (which the supervisor overwrites) and of any
// share-link fragment. Two links that differ only in name therefore
// dedupe. json.Marshal emits map keys sorted, so the canonical form is
// deterministic across parses.
func Fingerprint(outbound string) string {
	var ob map[string]any
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		sum := sha256.Sum256([]byte(strings.TrimSpace(outbound)))
		return hex.EncodeToString(sum[:])[:16]
	}
	delete(ob, "tag")
	canon, err := json.Marshal(ob)
	if err != nil {
		sum := sha256.Sum256([]byte(strings.TrimSpace(outbound)))
		return hex.EncodeToString(sum[:])[:16]
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])[:16]
}

// validateSub normalizes and checks a subscription before write.
func validateSub(sub *Subscription) error {
	sub.Name = normalizeName(sub.Name)
	if !ValidName(sub.Name) {
		return ErrInvalidName
	}
	sub.URL = strings.TrimSpace(sub.URL)
	u, err := url.Parse(sub.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidSubURL
	}
	sub.UserAgent = strings.TrimSpace(sub.UserAgent)
	if sub.IntervalSec < 0 {
		sub.IntervalSec = 0
	}
	if sub.NodeCap < 0 {
		sub.NodeCap = 0
	}
	return nil
}

// EffectiveInterval returns the refresh cadence, applying the default.
func (sub Subscription) EffectiveInterval() time.Duration {
	if sub.IntervalSec <= 0 {
		return time.Duration(DefaultSubIntervalSec) * time.Second
	}
	return time.Duration(sub.IntervalSec) * time.Second
}

// EffectiveCap returns the active-node cap, applying the default.
func (sub Subscription) EffectiveCap() int {
	if sub.NodeCap <= 0 {
		return DefaultSubNodeCap
	}
	return sub.NodeCap
}

// AddSub inserts a subscription. Enforces name uniqueness.
func (s *Store) AddSub(ctx context.Context, sub Subscription) (Subscription, error) {
	if err := validateSub(&sub); err != nil {
		return Subscription{}, err
	}
	sub.ID = 0
	now := time.Now().UTC()
	sub.CreatedAt = now
	sub.UpdatedAt = now
	if err := s.db.WithContext(ctx).Create(&sub).Error; err != nil {
		if isUniqueErr(err) {
			return Subscription{}, ErrDuplicate
		}
		return Subscription{}, fmt.Errorf("insert subscription: %w", err)
	}
	return sub, nil
}

// UpdateSub overwrites the user-editable fields (name, url, ua, interval,
// cap, enabled). LastFetched / LastError are managed by SetSubFetchResult
// and are not touched here.
func (s *Store) UpdateSub(ctx context.Context, sub Subscription) error {
	if err := validateSub(&sub); err != nil {
		return err
	}
	sub.UpdatedAt = time.Now().UTC()
	fields := map[string]any{
		"name":         sub.Name,
		"url":          sub.URL,
		"user_agent":   sub.UserAgent,
		"interval_sec": sub.IntervalSec,
		"node_cap":     sub.NodeCap,
		"enabled":      sub.Enabled,
		"updated_at":   sub.UpdatedAt,
	}
	res := s.db.WithContext(ctx).Model(&Subscription{}).Where("id = ?", sub.ID).Updates(fields)
	if res.Error != nil {
		if isUniqueErr(res.Error) {
			return ErrDuplicate
		}
		return fmt.Errorf("update subscription: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	// Enable/cap changes affect which nodes are active.
	return s.recomputeActive(ctx, sub.ID)
}

// DeleteSub removes a subscription and all of its nodes and overrides.
func (s *Store) DeleteSub(ctx context.Context, id int64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("sub_id = ?", id).Delete(&SubNode{}).Error; err != nil {
			return err
		}
		if err := tx.Where("sub_id = ?", id).Delete(&SubNodeOverride{}).Error; err != nil {
			return err
		}
		res := tx.Delete(&Subscription{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) GetSub(ctx context.Context, id int64) (Subscription, error) {
	var sub Subscription
	err := s.db.WithContext(ctx).First(&sub, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Subscription{}, ErrNotFound
	}
	return sub, err
}

func (s *Store) GetSubByName(ctx context.Context, name string) (Subscription, error) {
	n := normalizeName(name)
	if n == "" {
		return Subscription{}, ErrNotFound
	}
	var sub Subscription
	err := s.db.WithContext(ctx).First(&sub, "name = ?", n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Subscription{}, ErrNotFound
	}
	return sub, err
}

func (s *Store) ListSubs(ctx context.Context) ([]Subscription, error) {
	var out []Subscription
	err := s.db.WithContext(ctx).Order("name ASC").Find(&out).Error
	return out, err
}

// SubNamesExist reports which requested subscription names are missing,
// mirroring Store.NamesExist so a master's xraysub: dialer refs can be
// validated at write time.
func (s *Store) SubNamesExist(ctx context.Context, names []string) (missing []string, err error) {
	if len(names) == 0 {
		return nil, nil
	}
	canon := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = normalizeName(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		canon = append(canon, n)
	}
	var rows []Subscription
	if err := s.db.WithContext(ctx).Select("name").
		Where("name IN ?", canon).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("lookup subscriptions: %w", err)
	}
	have := make(map[string]bool, len(rows))
	for _, r := range rows {
		have[r.Name] = true
	}
	for _, n := range canon {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// SetSubFetchResult records the outcome of a fetch attempt. errMsg empty
// means success. Always stamps LastFetched.
func (s *Store) SetSubFetchResult(ctx context.Context, id int64, errMsg string) error {
	res := s.db.WithContext(ctx).Model(&Subscription{}).Where("id = ?", id).
		Updates(map[string]any{
			"last_fetched": time.Now().UTC(),
			"last_error":   strings.TrimSpace(errMsg),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceNodes swaps the entire node set for a subscription in one
// transaction, then recomputes which nodes are active (respecting stored
// overrides and the cap). Callers pass nodes as produced by
// ParseSubscriptionBody (Fingerprint + Name already set); SubID is forced
// to subID here.
func (s *Store) ReplaceNodes(ctx context.Context, subID int64, nodes []SubNode) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("sub_id = ?", subID).Delete(&SubNode{}).Error; err != nil {
			return err
		}
		for i := range nodes {
			n := nodes[i]
			n.ID = 0
			n.SubID = subID
			if n.LastLatencyMs == 0 {
				n.LastLatencyMs = -1
			}
			if err := tx.Create(&n).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replace nodes: %w", err)
	}
	return s.recomputeActive(ctx, subID)
}

// ListNodes returns all nodes for a subscription in insertion order.
func (s *Store) ListNodes(ctx context.Context, subID int64) ([]SubNode, error) {
	var out []SubNode
	err := s.db.WithContext(ctx).Where("sub_id = ?", subID).Order("id ASC").Find(&out).Error
	return out, err
}

// ActiveNodesForSubs returns the active nodes across the named
// subscriptions, in (subscription-name, insertion) order — the pool a
// master dialer draws from. Unknown names are ignored.
func (s *Store) ActiveNodesForSubs(ctx context.Context, names []string) ([]SubNode, error) {
	var out []SubNode
	for _, name := range names {
		sub, err := s.GetSubByName(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var nodes []SubNode
		if err := s.db.WithContext(ctx).
			Where("sub_id = ? AND active = ?", sub.ID, true).
			Order("id ASC").Find(&nodes).Error; err != nil {
			return nil, err
		}
		out = append(out, nodes...)
	}
	return out, nil
}

// SetNodeDisabled upserts an override for (subID, fingerprint) and
// recomputes active flags. disabled=false clears the override.
func (s *Store) SetNodeDisabled(ctx context.Context, subID int64, fingerprint string, disabled bool) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ErrNotFound
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ov SubNodeOverride
		err := tx.First(&ov, "sub_id = ? AND fingerprint = ?", subID, fingerprint).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			if !disabled {
				return nil // nothing to clear
			}
			return tx.Create(&SubNodeOverride{SubID: subID, Fingerprint: fingerprint, Disabled: true}).Error
		case err != nil:
			return err
		default:
			if !disabled {
				return tx.Delete(&SubNodeOverride{}, ov.ID).Error
			}
			return tx.Model(&SubNodeOverride{}).Where("id = ?", ov.ID).Update("disabled", true).Error
		}
	})
	if err != nil {
		return fmt.Errorf("set node disabled: %w", err)
	}
	return s.recomputeActive(ctx, subID)
}

// SetNodeLatency records the last measured latency (ms) for a node,
// keyed by fingerprint. A negative value means "unknown".
func (s *Store) SetNodeLatency(ctx context.Context, subID int64, fingerprint string, ms int) error {
	res := s.db.WithContext(ctx).Model(&SubNode{}).
		Where("sub_id = ? AND fingerprint = ?", subID, fingerprint).
		Update("last_latency_ms", ms)
	return res.Error
}

// SetSubEnabled toggles a subscription and recomputes active flags (a
// disabled subscription contributes no active nodes).
func (s *Store) SetSubEnabled(ctx context.Context, id int64, enabled bool) error {
	res := s.db.WithContext(ctx).Model(&Subscription{}).Where("id = ?", id).Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return s.recomputeActive(ctx, id)
}

// DisabledFingerprints returns the set of fingerprints manually disabled
// for a subscription.
func (s *Store) DisabledFingerprints(ctx context.Context, subID int64) (map[string]bool, error) {
	var ov []SubNodeOverride
	if err := s.db.WithContext(ctx).
		Where("sub_id = ? AND disabled = ?", subID, true).Find(&ov).Error; err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(ov))
	for _, o := range ov {
		m[o.Fingerprint] = true
	}
	return m, nil
}

// CountNodes returns total and active node counts for a subscription.
func (s *Store) CountNodes(ctx context.Context, subID int64) (total, active int, err error) {
	var nodes []SubNode
	if err = s.db.WithContext(ctx).Select("active").Where("sub_id = ?", subID).Find(&nodes).Error; err != nil {
		return 0, 0, err
	}
	total = len(nodes)
	for _, n := range nodes {
		if n.Active {
			active++
		}
	}
	return total, active, nil
}

// RenameDialerSubRef rewrites every master entry's Dialer field so an
// "xraysub:oldName" ref becomes "xraysub:newName" — the cascade run when a
// subscription is renamed. RenameDialerXrayRef is the same for "xray:"
// refs, run when an entry is renamed. Both mirror
// rules.Store.RenameInterfaceRef, dedupe on collision, apply atomically
// (all rewritten rows or none), and return the number of entries updated.
func (s *Store) RenameDialerSubRef(ctx context.Context, oldName, newName string) (int, error) {
	return s.renameDialerRef(ctx, DialerKindXraysub, oldName, newName)
}

func (s *Store) RenameDialerXrayRef(ctx context.Context, oldName, newName string) (int, error) {
	return s.renameDialerRef(ctx, DialerKindXray, oldName, newName)
}

func (s *Store) renameDialerRef(ctx context.Context, kind, oldName, newName string) (int, error) {
	oldName = normalizeName(oldName)
	newName = normalizeName(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return 0, nil
	}
	all, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	type update struct {
		id     int64
		dialer string
	}
	var updates []update
	for _, c := range all {
		refs, perr := ParseDialer(c.Dialer)
		if perr != nil || len(refs) == 0 {
			continue
		}
		changed := false
		seen := make(map[string]bool, len(refs))
		out := make([]DialerRef, 0, len(refs))
		for _, r := range refs {
			if r.Kind == kind && r.Name == oldName {
				r.Name = newName
				changed = true
			}
			key := r.Kind + ":" + r.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
		if changed {
			updates = append(updates, update{c.ID, FormatDialer(out)})
		}
	}
	if len(updates) == 0 {
		return 0, nil
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, u := range updates {
			if err := tx.Model(&Config{}).Where("id = ?", u.id).Update("dialer", u.dialer).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(updates), nil
}

// AllNodeCounts returns per-subscription (total, active) node counts in a
// single grouped query — avoids an N+1 in the subscription list.
func (s *Store) AllNodeCounts(ctx context.Context) (map[int64][2]int, error) {
	type row struct {
		SubID  int64
		Total  int
		Active int
	}
	var rows []row
	if err := s.db.WithContext(ctx).Model(&SubNode{}).
		Select("sub_id, count(*) as total, sum(case when active then 1 else 0 end) as active").
		Group("sub_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[int64][2]int, len(rows))
	for _, r := range rows {
		m[r.SubID] = [2]int{r.Total, r.Active}
	}
	return m, nil
}

// recomputeActive re-derives every node's Active flag for a subscription:
// active = subscription enabled AND not overridden-off AND within the
// first EffectiveCap() non-disabled nodes (insertion order).
func (s *Store) recomputeActive(ctx context.Context, subID int64) error {
	sub, err := s.GetSub(ctx, subID)
	if err != nil {
		return err
	}
	var overrides []SubNodeOverride
	if err := s.db.WithContext(ctx).
		Where("sub_id = ? AND disabled = ?", subID, true).Find(&overrides).Error; err != nil {
		return err
	}
	disabled := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		disabled[o.Fingerprint] = true
	}
	var nodes []SubNode
	if err := s.db.WithContext(ctx).Where("sub_id = ?", subID).Order("id ASC").Find(&nodes).Error; err != nil {
		return err
	}
	cap := sub.EffectiveCap()
	count := 0
	for i := range nodes {
		active := false
		if sub.Enabled && !disabled[nodes[i].Fingerprint] && count < cap {
			active = true
			count++
		}
		if nodes[i].Active != active {
			if err := s.db.WithContext(ctx).Model(&SubNode{}).
				Where("id = ?", nodes[i].ID).Update("active", active).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
