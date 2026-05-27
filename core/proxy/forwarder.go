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
// Returns the number of bytes copied in each direction (a->b, b->a)
// and the first non-nil error from either direction.
func Splice(a, b net.Conn) (atob, btoa int64, err error) {
	type result struct {
		n   int64
		err error
	}
	c1, c2 := make(chan result, 1), make(chan result, 1)

	go func() {
		n, e := copyAndCloseWrite(b, a)
		c1 <- result{n, e}
	}()
	go func() {
		n, e := copyAndCloseWrite(a, b)
		c2 <- result{n, e}
	}()

	r1 := <-c1
	r2 := <-c2
	atob, btoa = r1.n, r2.n
	if r1.err != nil && r1.err != io.EOF {
		err = r1.err
	} else if r2.err != nil && r2.err != io.EOF {
		err = r2.err
	}
	return
}

// copyAndCloseWrite copies src -> dst, then half-closes the write
// side of dst so the peer sees a clean FIN. Falls back to a full
// Close if the conn doesn't expose CloseWrite (e.g. a TLS wrapper).
func copyAndCloseWrite(dst, src net.Conn) (int64, error) {
	n, err := io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.SetWriteDeadline(time.Now())
	}
	return n, err
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
