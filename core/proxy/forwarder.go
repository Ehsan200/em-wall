package proxy

import (
	"io"
	"net"
	"sync"
	"time"
)

// SpliceTimeout caps half-duplex idle time on a TCP splice. If neither
// direction has traffic for this long we tear both sides down so a
// half-closed connection in either direction doesn't leak resources.
// Set to a value that's friendly to long-lived connections (chat,
// SSH) but short enough to reclaim dead sockets in a reasonable time.
const SpliceTimeout = 5 * time.Minute

// Splice copies bytes between two TCP-like conns in both directions
// until one side returns EOF or errors. Returns after both directions
// have finished. The caller is responsible for closing both conns
// (Splice closes neither — but it does call CloseWrite to half-close
// each side as its source EOFs, so the peer sees a clean FIN rather
// than a RST).
//
// A direction that EOFs cleanly leaves the other running (a half-closed
// download keeps streaming). A direction that hard-errors marks the whole
// session broken, so we force the other direction's read to expire now
// instead of waiting out its idle timeout — prompt, leak-free teardown.
//
// Returns the number of bytes copied in each direction (a->b, b->a)
// and the first non-nil error from either direction.
func Splice(a, b net.Conn) (atob, btoa int64, err error) {
	type result struct {
		dir int // 0 = a->b, 1 = b->a
		n   int64
		err error
	}
	done := make(chan result, 2)

	go func() {
		n, e := copyAndCloseWrite(b, a)
		done <- result{0, n, e}
	}()
	go func() {
		n, e := copyAndCloseWrite(a, b)
		done <- result{1, n, e}
	}()

	for i := 0; i < 2; i++ {
		r := <-done
		if r.dir == 0 {
			atob = r.n
		} else {
			btoa = r.n
		}
		if err == nil && r.err != nil && r.err != io.EOF {
			err = r.err
			// Hard error: unblock the still-running direction immediately.
			_ = a.SetReadDeadline(time.Now())
			_ = b.SetReadDeadline(time.Now())
		}
	}
	return
}

// copyAndCloseWrite copies src -> dst with an idle read deadline, then
// half-closes the write side of dst so the peer sees a clean FIN. Falls
// back to a write-deadline poke if the conn doesn't expose CloseWrite
// (e.g. a TLS wrapper).
func copyAndCloseWrite(dst, src net.Conn) (int64, error) {
	n, err := idleCopy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.SetWriteDeadline(time.Now())
	}
	return n, err
}

// idleCopy copies src -> dst, resetting a SpliceTimeout read deadline on
// src before each read so a stalled or half-open connection can't block
// the goroutine forever. A clean EOF returns nil; an idle expiry returns
// the timeout error.
func idleCopy(dst, src net.Conn) (int64, error) {
	var total int64
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(SpliceTimeout))
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return total, nil
			}
			return total, rerr
		}
	}
}

// FullCloseOnce closes a conn at most once. Useful because Splice's
// caller typically wants to ensure both ends are closed after the
// goroutine finishes, but a deferred Close can race with the splice.
type FullCloseOnce struct {
	once sync.Once
	c    net.Conn
}

func NewFullCloseOnce(c net.Conn) *FullCloseOnce { return &FullCloseOnce{c: c} }
func (f *FullCloseOnce) Close() error {
	var err error
	f.once.Do(func() { err = f.c.Close() })
	return err
}
