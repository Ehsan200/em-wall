package netprobe

import (
	"bufio"
	"context"
	"fmt"
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
