package main

import (
	"sort"
	"sync"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

// Master slot shortlist.
//
// A dialer slot's leastLoad balancer ranks its members by the standard
// deviation of their observatory pings, and only then by average RTT
// (xray-core app/router/strategy_leastload.go). So "the best two" it
// spreads over are the two steadiest, not the two fastest: a node pinging a
// flat 700ms beats one at 120ms ± 20ms. On a pool of a dozen nodes that can
// leave every master tunnelled through a slow node while fast ones sit idle.
//
// The daemon already reads the same observatory window every
// nodeHealthPollInterval (for parking), so it ranks the members itself —
// average plus deviation, inflated by the failure rate — and names the best
// xray.SlotShortlistSize as the slot's Preferred. Generate turns that into
// leastLoad costs that sort every other member far behind, which keeps
// xray's own ping-by-ping failover: a shortlisted node that stops answering
// is passed over on its next failed pings, without waiting for the next
// poll here. Only the routing section changes, so a new shortlist applies
// live and open connections are untouched.
//
// The shortlist is sticky. A member keeps its place while it qualifies,
// and a challenger only displaces the weakest one when it is better by both
// margins — the same idea as the set ranking's hysteresis (core/netprobe),
// since every change is a live routing swap and near-equal nodes would
// otherwise trade places on every poll.

const (
	// shortlistMinPings is how many pings a member's window must hold
	// before it is scored: a node fresh out of parking, or just added by a
	// subscription refresh, has one or two, and one good ping says little.
	shortlistMinPings = 3

	// Displacement margins (see the file comment): a challenger must beat
	// the weakest shortlisted member by this fraction AND this much.
	shortlistMarginFraction = 0.25
	shortlistMarginFloor    = 50 * time.Millisecond

	// shortlistFailWeight inflates a member's score by its window failure
	// rate, like netprobe's failRateWeight: a node that loses one ping in
	// six is worth less than its RTT says, since every lost ping is a
	// connection that stalled.
	shortlistFailWeight = 2.0
)

// nodeScore is a member's shortlist cost: lower is better. ok is false for
// a member that can't be scored or doesn't qualify — too few pings, or
// failing beyond the balancer's own tolerance. Qualification is judged on
// the window, not on the alive flag, so one lost ping doesn't cost an
// incumbent its seat.
func nodeScore(st nodeStatus) (time.Duration, bool) {
	hp := st.HealthPing
	if hp.All < shortlistMinPings || hp.Fail >= hp.All {
		return 0, false
	}
	rate := float64(hp.Fail) / float64(hp.All)
	if rate > xray.SlotBalancerTolerance {
		return 0, false
	}
	avg := time.Duration(hp.Average)
	if avg <= 0 {
		avg = time.Duration(st.Delay) * time.Millisecond
	}
	if avg <= 0 {
		return 0, false
	}
	// Jitter counts once, added to the average: it is what a connection's
	// handshake meets on a bad draw, not a ranking key of its own.
	base := avg + time.Duration(hp.Deviation)
	return time.Duration(float64(base) * (1 + shortlistFailWeight*rate)), true
}

// slotShortlist holds each slot's current shortlist, keyed by the slot's
// owning master (stable across reconciles: the owner is the first master,
// by name, of its dialer group). Safe on a nil receiver, which never
// shortlists.
type slotShortlist struct {
	mu    sync.Mutex
	picks map[string][]string // owner master → member keys, best first
}

func newSlotShortlist() *slotShortlist {
	return &slotShortlist{picks: map[string][]string{}}
}

// preferred returns master's shortlist for a slot with these members, or
// nil when there is none or the slot is too small for one to matter.
func (sl *slotShortlist) preferred(master string, members []xray.DialerMember) []string {
	if sl == nil || len(members) <= xray.SlotShortlistSize {
		return nil
	}
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return append([]string(nil), sl.picks[master]...)
}

// shortlistChange describes one slot whose shortlist moved, for logging.
type shortlistChange struct {
	master   string
	from, to []string
}

// observe re-ranks every loaded slot from one round of observatory status
// and returns the slots whose shortlist changed.
func (sl *slotShortlist) observe(slots []xray.DialerSlot, byTag map[string]nodeStatus) []shortlistChange {
	if sl == nil {
		return nil
	}
	sl.mu.Lock()
	defer sl.mu.Unlock()
	var changes []shortlistChange
	live := make(map[string]bool, len(slots))
	for _, slot := range slots {
		live[slot.Master] = true
		old := sl.picks[slot.Master]
		if len(slot.Members) <= xray.SlotShortlistSize {
			if len(old) > 0 {
				delete(sl.picks, slot.Master)
				changes = append(changes, shortlistChange{master: slot.Master, from: old})
			}
			continue
		}
		scores := make(map[string]time.Duration, len(slot.Members))
		for _, m := range slot.Members {
			if st, ok := byTag[xray.SlotMemberTag(slot.Index, m.Key)]; ok {
				if c, ok := nodeScore(st); ok {
					scores[m.Key] = c
				}
			}
		}
		next := pickShortlist(old, scores, xray.SlotShortlistSize)
		if next == nil || equalStrings(next, old) {
			continue
		}
		sl.picks[slot.Master] = next
		changes = append(changes, shortlistChange{master: slot.Master, from: old, to: next})
	}
	for m := range sl.picks {
		if !live[m] {
			delete(sl.picks, m)
		}
	}
	return changes
}

// pickShortlist returns the next shortlist of up to size keys given the
// current one and this round's qualified scores, or nil to leave the
// current one as it is. With nothing qualified it is left alone: every
// member failing at once is the uplink, not the pool, and a shortlist
// emptied now would have to be rebuilt from scratch once the link returns.
// An incumbent that no longer qualifies loses its seat at once.
func pickShortlist(old []string, scores map[string]time.Duration, size int) []string {
	if len(scores) == 0 {
		return nil
	}
	// Incumbents that still qualify keep their place and their order.
	next := make([]string, 0, size)
	in := map[string]bool{}
	for _, k := range old {
		if _, ok := scores[k]; ok && len(next) < size {
			next = append(next, k)
			in[k] = true
		}
	}
	// Outsiders, best first; ties break by key so map order never matters.
	var outs []string
	for k := range scores {
		if !in[k] {
			outs = append(outs, k)
		}
	}
	sort.Slice(outs, func(i, j int) bool {
		a, b := outs[i], outs[j]
		if scores[a] != scores[b] {
			return scores[a] < scores[b]
		}
		return a < b
	})
	// Free seats are filled unconditionally…
	for len(next) < size && len(outs) > 0 {
		next = append(next, outs[0])
		outs = outs[1:]
	}
	// …then the best outsider displaces the weakest incumbent while it
	// clearly beats it. Outsiders are sorted, so the first one that doesn't
	// is the end of it.
	for len(outs) > 0 {
		weak := 0
		for i, k := range next {
			if scores[k] > scores[next[weak]] {
				weak = i
			}
		}
		if !beatsShortlisted(scores[outs[0]], scores[next[weak]]) {
			break
		}
		next[weak] = outs[0]
		outs = outs[1:]
	}
	// The head is the balancer's fallback, so put the best member there —
	// but only when it is clearly best, or two near-equal members would
	// swap the fallback (a routing change) on every poll.
	best := 0
	for i, k := range next {
		if scores[k] < scores[next[best]] {
			best = i
		}
	}
	if best != 0 && beatsShortlisted(scores[next[best]], scores[next[0]]) {
		next[0], next[best] = next[best], next[0]
	}
	return next
}

// beatsShortlisted reports whether challenger is better than incumbent by
// both displacement margins.
func beatsShortlisted(challenger, incumbent time.Duration) bool {
	gain := incumbent - challenger
	return gain >= shortlistMarginFloor && float64(gain) >= shortlistMarginFraction*float64(incumbent)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
