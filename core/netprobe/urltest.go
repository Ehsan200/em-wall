package netprobe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// URL test ("real delay"), the probe V2Box, Hiddify, sing-box and Clash
// rank outbounds by: a plain HTTP GET for a generate_204 page through the
// connector, timed from the dial to the response's status line, and a
// success only on 204. Unlike a bare TLS handshake it proves a full request
// and response crossed the whole chain to a real destination and back —
// a server that answers the handshake but stalls the request no longer
// passes — and it costs one round trip less, so the figure is close to
// what a page's first request sees.
//
// Plain HTTP on purpose: the request travels inside the tunnel, so the
// local network sees nothing, and the exit-side round trip is all that is
// measured.
const (
	URLTestHost = "cp.cloudflare.com"
	URLTestPath = "/generate_204"
	URLTestPort = 80
)

// MeasureURL runs one URL test through c against host:port/path. The
// connection is always closed before returning.
func MeasureURL(ctx context.Context, c Connector, host string, port int, path string) Result {
	start := time.Now()
	conn, err := c.Dial(ctx, host, nil, port)
	if err != nil {
		return Result{Stage: StageDial, Err: err}
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	_ = conn.SetDeadline(deadline)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: em-wall\r\nConnection: close\r\n\r\n", path, host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return Result{Stage: StageTLS, Err: err}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return Result{Stage: StageTLS, Err: err}
	}
	latency := time.Since(start)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return Result{Stage: StageTLS, Err: fmt.Errorf("url test: status %d, want 204", resp.StatusCode)}
	}
	return Result{OK: true, Latency: latency}
}

// Transfer test: a URL test proves a request crossed the chain, but its
// reply is a few hundred bytes. Some paths pass that and still choke every
// real connection — observed on a home ISP, a raw-TCP VLESS node moved
// 7–13 KB per connection and then stalled, while it answered generate_204
// in 300ms. So the path prober also pulls a body larger than any such
// cut-off and passes only when all of it arrives in time.
const (
	DownloadTestHost  = "speed.cloudflare.com"
	DownloadTestPort  = 80
	DownloadTestBytes = 64 << 10
)

// DownloadTestPath is the path that serves n bytes on DownloadTestHost.
func DownloadTestPath(n int) string { return fmt.Sprintf("/__down?bytes=%d", n) }

// MeasureDownload GETs path through c and reads the body. It succeeds only
// when the status is 200 and at least want bytes arrive before ctx's
// deadline; Latency is the time to the last byte. The connection is always
// closed before returning.
func MeasureDownload(ctx context.Context, c Connector, host string, port int, path string, want int) Result {
	start := time.Now()
	conn, err := c.Dial(ctx, host, nil, port)
	if err != nil {
		return Result{Stage: StageDial, Err: err}
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	_ = conn.SetDeadline(deadline)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: em-wall\r\nConnection: close\r\n\r\n", path, host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return Result{Stage: StageTLS, Err: err}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return Result{Stage: StageTLS, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{Stage: StageTLS, Err: fmt.Errorf("download test: status %d, want 200", resp.StatusCode)}
	}
	n, err := io.CopyN(io.Discard, resp.Body, int64(want))
	if n < int64(want) {
		return Result{Stage: StageTLS, Err: fmt.Errorf("download test: stalled after %d of %d bytes: %v", n, want, err)}
	}
	return Result{OK: true, Latency: time.Since(start)}
}
