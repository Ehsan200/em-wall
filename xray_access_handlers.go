package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

// xrayAccessLogName is the access log Generate points xray at (see
// xrayLogFiles).
const xrayAccessLogName = "em-wall-xray-access.log"

// registerXrayActiveHandlers sets up the access-log pick tracker and
// registers xray.active. The follower itself is started by
// runXrayAccessLog once the daemon's run context exists.
func registerXrayActiveHandlers(s *ipc.Server, d *handlerDeps) {
	if sup := d.xraySup; sup != nil && sup.Enabled() && sup.logDir != "" {
		activeXrayAccess.Store(newXrayAccessLog(filepath.Join(sup.logDir, xrayAccessLogName)))
	}
	svc := &xrayActiveService{d: d}
	s.Handle(ipc.MethodXrayActive, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ipc.XrayActiveParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
		}
		return svc.active(ctx, activeXrayAccess.Load(), p), nil
	})
}

// xrayActiveService answers xray.active from memory; only node names are
// read from the store, and cached.
type xrayActiveService struct {
	d       *handlerDeps
	mu      sync.Mutex
	names   map[string]string // pool member key → display name
	namesAt time.Time
}

func (v *xrayActiveService) nodeNames(ctx context.Context, now time.Time) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.names == nil || now.Sub(v.namesAt) >= mapNodeNamesTTL {
		v.names, v.namesAt = poolNodeNamesFrom(ctx, v.d.xrayStore), now
	}
	return v.names
}

func (v *xrayActiveService) active(ctx context.Context, a *xrayAccessLog, p ipc.XrayActiveParams) ipc.XrayActiveResult {
	window := accessWindow
	if p.WindowSec > 0 && time.Duration(p.WindowSec)*time.Second < window {
		window = time.Duration(p.WindowSec) * time.Second
	}
	following, note := a.status()
	now := time.Now()
	if a != nil {
		now = a.now()
	}
	res := ipc.XrayActiveResult{
		Following: following, Note: note,
		WindowSec: int(window / time.Second), RecentSec: int(min(window, accessRecent) / time.Second),
		GeneratedAt: now.UnixMilli(),
		Pools:       []ipc.XrayActivePoolDTO{}, Routes: []ipc.XrayActiveRouteDTO{},
	}
	if a == nil {
		return res
	}
	var slots []xray.DialerSlot
	if sup := v.d.xraySup; sup != nil {
		sup.mu.Lock()
		slots = sup.loadedSlots
		sup.mu.Unlock()
	}
	names := v.nodeNames(ctx, now)
	hist := 0
	if p.History {
		hist = accessHistCap
	}
	res.Pools = buildActivePools(a, slots, names, window, hist, now)
	res.Routes = buildActiveRoutes(a.inboundPicks(window), slots, names, now)
	return res
}

// slotTagKeys maps each of slot's member outbound tags to its member key
// (tags carry a sanitized key, so SlotMemberKey alone can't give it back).
func slotTagKeys(slot xray.DialerSlot) map[string]string {
	out := make(map[string]string, len(slot.Members))
	for _, m := range slot.Members {
		out[xray.SlotMemberTag(slot.Index, m.Key)] = m.Key
	}
	return out
}

func nodeLabel(names map[string]string, key string) string {
	if n := names[key]; n != "" {
		return n
	}
	return memberDisplayName(key)
}

func buildActivePools(a *xrayAccessLog, slots []xray.DialerSlot, names map[string]string, window time.Duration, hist int, now time.Time) []ipc.XrayActivePoolDTO {
	out := []ipc.XrayActivePoolDTO{}
	for _, sl := range slots {
		picks, start := a.slotPicks(sl, window, hist)
		keyOf := slotTagKeys(sl)
		dto := ipc.XrayActivePoolDTO{
			Master: sl.Master, Masters: sl.SlotMasters(), Slot: sl.Index,
			Members: []ipc.XrayActiveMemberDTO{},
		}
		if hist > 0 {
			dto.BucketSec, dto.HistoryStart = int(accessHistBucket/time.Second), start
		}
		dto.Members, dto.Picks, dto.Recent = memberDTOs(picks, func(tag string) (string, string) {
			k := keyOf[tag]
			return k, nodeLabel(names, k)
		}, now)
		if len(picks) > 0 && (picks[0].recent > 0 || picks[0].picks > 0) {
			dto.Active, dto.ActiveName = dto.Members[0].Key, dto.Members[0].Name
			dto.Dominant = dominant(picks)
		}
		out = append(out, dto)
	}
	return out
}

// memberDTOs renders sorted picks; ident gives each tag's key and name.
func memberDTOs(picks []outPick, ident func(tag string) (key, name string), now time.Time) (ms []ipc.XrayActiveMemberDTO, total, recent int) {
	ms = []ipc.XrayActiveMemberDTO{}
	for _, p := range picks {
		total += p.picks
		recent += p.recent
	}
	for _, p := range picks {
		k, n := ident(p.tag)
		m := ipc.XrayActiveMemberDTO{
			Key: k, Name: n, Picks: p.picks, Recent: p.recent,
			Active: p.recent > 0, History: p.history,
		}
		if total > 0 {
			m.SharePct = math.Round(1000*float64(p.picks)/float64(total)) / 10
		}
		if !p.last.IsZero() {
			m.LastPicked = p.last.UnixMilli()
			m.AgoSec = math.Round(10*now.Sub(p.last).Seconds()) / 10
		}
		ms = append(ms, m)
	}
	return ms, total, recent
}

// buildActiveRoutes renders each entry inbound's picks, mapping outbound
// tags back to what the UI calls them.
func buildActiveRoutes(byInbound map[string][]outPick, slots []xray.DialerSlot, names map[string]string, now time.Time) []ipc.XrayActiveRouteDTO {
	nodeOf := map[string]string{} // slot member tag → display name
	for _, sl := range slots {
		for tag, k := range slotTagKeys(sl) {
			nodeOf[tag] = nodeLabel(names, k)
		}
	}
	display := func(tag string) string {
		switch {
		case tag == xray.TagDirect || tag == xray.TagBlock:
			return tag
		case strings.HasPrefix(tag, "out-"):
			return strings.TrimPrefix(tag, "out-")
		case nodeOf[tag] != "":
			return nodeOf[tag]
		case strings.HasPrefix(tag, "dialer-"):
			return strings.TrimPrefix(tag, "dialer-") + " (dialer)"
		}
		return tag
	}
	out := []ipc.XrayActiveRouteDTO{}
	for in, picks := range byInbound {
		entry := strings.TrimPrefix(in, "in-")
		r := ipc.XrayActiveRouteDTO{Entry: entry, Inbound: in}
		r.Members, r.Picks, r.Recent = memberDTOs(picks, func(tag string) (string, string) { return tag, display(tag) }, now)
		r.Active, r.ActiveName = r.Members[0].Key, r.Members[0].Name
		r.Diverted = len(picks) > 1 || picks[0].tag != xray.OutboundTag(entry)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Entry < out[j].Entry })
	return out
}
