package main

import (
	"sync"
	"time"
)

// Per-connection log sampling.
//
// "proxytun: <host>:<port> via proxy <name>" is written once per accepted
// connection. That is the right granularity when connections arrive at
// human speed and the wrong one at any other: during the storm tcpHealth
// now brakes, this single line was emitted six thousand times a minute for
// one hostname. The cost is not just disk — log.Logger serialises every
// caller on one mutex and every line is an unbuffered write to a file, so
// a connection storm turns into a thread storm through the logger alone.
//
// Sampling keeps what the line is actually read for. Every distinct
// destination still appears, and still appears the first time it is seen,
// which is what makes the log useful for "where did this request go".
// What it drops is the thousandth repetition of a tuple already on the
// line above, replaced by a count so the volume stays visible.
const (
	// logSampleInterval is the window a key is quiet for after being
	// logged. Long enough to flatten a storm, short enough that a steady
	// stream still leaves a trail through the day.
	logSampleInterval = 30 * time.Second

	// logSampleEntryTTL prunes keys nothing has hit. Pruning is
	// piggybacked on writes, so this caps the work rather than scheduling
	// it — the same arrangement stickyBindings uses.
	logSampleEntryTTL = 10 * time.Minute
)

type logSampleEntry struct {
	last       time.Time
	suppressed int
}

// logSampler rate-limits repeated log lines per key. Thread-safe; a nil
// *logSampler is usable and simply never suppresses anything.
type logSampler struct {
	mu        sync.Mutex
	entries   map[string]*logSampleEntry
	lastSweep time.Time
	now       func() time.Time // swappable for tests
}

func newLogSampler() *logSampler {
	return &logSampler{entries: make(map[string]*logSampleEntry), now: time.Now}
}

// allow reports whether key should be logged now, and how many occurrences
// were suppressed since it last was. A true return resets that count, so
// the caller must log when it gets one — dropping the line would lose the
// suppressed tally with it.
func (s *logSampler) allow(key string) (bool, int) {
	if s == nil {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	e := s.entries[key]
	if e == nil {
		s.entries[key] = &logSampleEntry{last: now}
		s.sweepLocked(now)
		return true, 0
	}
	if now.Sub(e.last) < logSampleInterval {
		e.suppressed++
		return false, 0
	}
	n := e.suppressed
	e.suppressed = 0
	e.last = now
	return true, n
}

// sweepLocked drops keys nothing has hit for logSampleEntryTTL, at most
// once per logSampleInterval.
func (s *logSampler) sweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < logSampleInterval {
		return
	}
	s.lastSweep = now
	for k, e := range s.entries {
		if now.Sub(e.last) > logSampleEntryTTL {
			delete(s.entries, k)
		}
	}
}
