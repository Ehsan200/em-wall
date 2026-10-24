package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"syscall"
	"time"
)

// Following xray's access log (see xray_access.go for what it holds).
//
// xray opens the file with O_APPEND and the supervisor trims it in place
// (truncateLocked, on a restart and when it passes the size cap), so the
// file keeps its inode and only ever shrinks to zero; a reinstall or a
// hand-deleted log recreates it under a new inode. The follower polls:
// cheap (one stat a second when idle), and nothing here may ever block
// xray — a blocked log write in xray stalls its dispatcher.

const (
	accessPollInterval = time.Second
	// accessReadChunk is how much one read takes; accessMaxBacklog is the
	// most the follower will catch up on in one poll. Past it (a storm
	// that logged megabytes in a second, or a long stall) the backlog is
	// skipped: picks that old are outside the window anyway.
	accessReadChunk  = 64 << 10
	accessMaxBacklog = 4 << 20
	// accessMaxLine bounds a partial line carried between reads; xray's
	// lines are ~150 bytes, so anything longer is not one of them.
	accessMaxLine = 16 << 10
	// accessPruneEvery spaces the sweep of idle counters.
	accessPruneEvery = time.Minute
)

// accessTail is the follower's file state; owned by one goroutine.
type accessTail struct {
	log     *xrayAccessLog
	f       *os.File
	ino     uint64
	off     int64
	partial []byte
	buf     []byte
	started bool // a first poll has run: a file seen from then on is new
}

func newAccessTail(a *xrayAccessLog) *accessTail {
	return &accessTail{log: a, buf: make([]byte, accessReadChunk)}
}

// runXrayAccessLog follows the daemon's access log until ctx ends. Call it
// once, after registerXrayActiveHandlers set the tracker; a nil tracker
// (xray off, no log dir) returns at once.
func runXrayAccessLog(ctx context.Context) {
	a := activeXrayAccess.Load()
	if a == nil || a.path == "" {
		return
	}
	t := newAccessTail(a)
	defer t.close()
	tick := time.NewTicker(accessPollInterval)
	defer tick.Stop()
	lastPrune := time.Now()
	for {
		t.poll()
		if now := time.Now(); now.Sub(lastPrune) >= accessPruneEvery {
			a.prune(now)
			lastPrune = now
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (t *accessTail) close() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	t.log.following.Store(false)
}

// poll reads whatever was appended since the last call. The first call
// starts at the end of an existing file — picks from before the daemon
// started are history, not "now" — while a file that appears (or is
// replaced) later is read from its start.
func (t *accessTail) poll() {
	first := !t.started
	t.started = true
	fi, err := os.Stat(t.log.path)
	if err != nil {
		t.close()
		t.log.setNote("xray access log not found: " + t.log.path)
		return
	}
	ino := fileIno(fi)
	if t.f == nil || ino != t.ino {
		if t.f != nil {
			_ = t.f.Close()
			t.f = nil
		}
		f, err := os.Open(t.log.path)
		if err != nil {
			t.log.following.Store(false)
			t.log.setNote("can't read xray access log: " + err.Error())
			return
		}
		t.f, t.ino, t.off, t.partial = f, ino, 0, t.partial[:0]
		if first {
			t.off = fi.Size()
		}
		t.log.following.Store(true)
		t.log.setNote("")
	}
	size := fi.Size()
	if size < t.off { // truncated in place
		t.off, t.partial = 0, t.partial[:0]
	}
	if size-t.off > accessMaxBacklog {
		t.off, t.partial = size, t.partial[:0]
	}
	now := t.log.now()
	for t.off < size {
		n, err := t.f.ReadAt(t.buf[:min(int64(len(t.buf)), size-t.off)], t.off)
		if n > 0 {
			t.off += int64(n)
			t.feed(t.buf[:n], now)
		}
		if err != nil && err != io.EOF {
			t.close()
			t.log.setNote("reading xray access log: " + err.Error())
			return
		}
		if n == 0 {
			break
		}
	}
}

// feed splits data into lines, carrying an unterminated tail to the next
// read. Every line read in one poll is stamped with the poll's time: one
// second of slack is far inside the window, and xray's own timestamps are
// local wall-clock text that would cost a parse per line.
func (t *accessTail) feed(data []byte, now time.Time) {
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			if len(t.partial)+len(data) <= accessMaxLine {
				t.partial = append(t.partial, data...)
			} else {
				t.partial = t.partial[:0]
			}
			return
		}
		line := data[:nl]
		if len(t.partial) > 0 {
			line = append(t.partial, line...)
			t.partial = t.partial[:0]
		}
		t.log.observe(line, now)
		data = data[nl+1:]
	}
}

// fileIno returns fi's inode; 0 where the platform doesn't say.
func fileIno(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
