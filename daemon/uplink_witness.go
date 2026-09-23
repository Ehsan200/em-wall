package main

import (
	"sync"
	"time"
)

// uplinkWitnessWindow is how recently another upstream must have carried
// data for a silent connection to count against its own upstream.
const uplinkWitnessWindow = 10 * time.Second

// uplinkWitness remembers when each upstream last proved it could carry
// data. It answers one question: when a connection sent bytes and heard
// nothing back, was the local uplink working at the time?
//
// If no OTHER upstream carried anything recently, the likeliest cause is
// the user's own link (or everything is idle), and striking the upstream
// that happened to be in use would demote members in lockstep — the same
// failure mode dialBinding avoids for total dial failures. If another
// upstream was carrying data, the link was fine and the silence is this
// upstream's fault.
type uplinkWitness struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newUplinkWitness() *uplinkWitness {
	return &uplinkWitness{last: map[string]time.Time{}, now: time.Now}
}

// saw records that name just carried data. Safe on a nil receiver.
func (w *uplinkWitness) saw(name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.last[name] = w.now()
	w.mu.Unlock()
}

// otherAlive reports whether an upstream other than name carried data
// within uplinkWitnessWindow. A nil receiver answers true, keeping the old
// always-blame behaviour where no witness is wired.
func (w *uplinkWitness) otherAlive(name string) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := w.now().Add(-uplinkWitnessWindow)
	for n, t := range w.last {
		if n != name && t.After(cutoff) {
			return true
		}
	}
	return false
}
