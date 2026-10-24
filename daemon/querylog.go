package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/ehsan/em-wall/core/rules"
)

const (
	// queryLogQueue bounds the entries waiting for a write. A DNS answer
	// never waits on the log: when the queue is full the entry is dropped
	// and counted — the log is a recent-activity view, not an audit trail.
	queryLogQueue = 4096
	// queryLogFlushEvery / queryLogFlushAt: write when either is reached.
	queryLogFlushEvery = 500 * time.Millisecond
	queryLogFlushAt    = 256
)

// storeLogSink is dnsproxy's query log, written behind the DNS path in
// batches. The store has one SQLite connection shared with the traffic
// drain, fake-IP leases and every IPC read; a per-query insert+trim made
// blocked/failed answers wait on whichever of those held it.
type storeLogSink struct {
	store   *rules.Store
	ch      chan rules.LogEntry
	dropped atomic.Int64
	// suggest, when set, is offered every name no rule matched (group
	// suggestions, via Unmatched). Nil-safe.
	suggest *suggester
}

func newStoreLogSink(store *rules.Store) *storeLogSink {
	return &storeLogSink{store: store, ch: make(chan rules.LogEntry, queryLogQueue)}
}

func (s *storeLogSink) Log(name, action, iface string, ruleID int64, clientIP string) {
	e := rules.LogEntry{
		Timestamp: time.Now().UTC(),
		QueryName: name,
		Action:    action,
		RuleID:    ruleID,
		Interface: iface,
		ClientIP:  clientIP,
	}
	select {
	case s.ch <- e:
	default:
		s.dropped.Add(1)
	}
}

// Unmatched is told every name the DNS proxy answered without a rule
// matching (plain allows are never logged, so Log doesn't see them). It
// feeds group suggestions: a non-blocking channel send, nothing more.
func (s *storeLogSink) Unmatched(name string) {
	s.suggest.Offer(name)
}

// run drains the queue until ctx ends, then writes what is left.
func (s *storeLogSink) run(ctx context.Context) {
	t := time.NewTicker(queryLogFlushEvery)
	defer t.Stop()
	batch := make([]rules.LogEntry, 0, queryLogFlushAt)
	flush := func() {
		if n := s.dropped.Swap(0); n > 0 {
			log.Printf("em-walld: query log: dropped %d entries (queue full)", n)
		}
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.store.LogBatch(wctx, batch); err != nil {
			log.Printf("em-walld: query log write: %v", err)
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case e := <-s.ch:
			batch = append(batch, e)
			if len(batch) >= queryLogFlushAt {
				flush()
			}
		case <-t.C:
			flush()
		case <-ctx.Done():
			for {
				select {
				case e := <-s.ch:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		}
	}
}
