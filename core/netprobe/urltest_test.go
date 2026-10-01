package netprobe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// loopConnector dials addr whatever it is asked for, like a proxy would.
type loopConnector struct{ addr string }

func (c loopConnector) Dial(ctx context.Context, _ string, _ net.IP, _ int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", c.addr)
}

func urlServer(t *testing.T, status int) loopConnector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != URLTestPath || r.Host != URLTestHost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return loopConnector{addr: srv.Listener.Addr().String()}
}

func TestMeasureURL204(t *testing.T) {
	c := urlServer(t, http.StatusNoContent)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := MeasureURL(ctx, c, URLTestHost, URLTestPort, URLTestPath)
	if !r.OK || r.Latency <= 0 {
		t.Fatalf("result = %+v, want OK with latency", r)
	}
}

// A captive portal or a broken exit answering something other than 204
// is not a working path.
func TestMeasureURLWrongStatusFails(t *testing.T) {
	c := urlServer(t, http.StatusOK)
	r := MeasureURL(context.Background(), c, URLTestHost, URLTestPort, URLTestPath)
	if r.OK {
		t.Fatalf("result = %+v, want failure on 200", r)
	}
}

// A server that accepts the connection but never answers times out.
func TestMeasureURLSilentFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	r := MeasureURL(ctx, loopConnector{addr: ln.Addr().String()}, URLTestHost, p, URLTestPath)
	if r.OK {
		t.Fatalf("result = %+v, want timeout failure", r)
	}
}
