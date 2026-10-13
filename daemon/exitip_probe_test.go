package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A network that filters the first prober (connects, never answers — as
// a mobile hotspot did with ip-api.com) must not leave the exit unknown:
// the next prober answers, well before the first one would time out.
func TestProbeExitIPFallsBackPastSilentProber(t *testing.T) {
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer silent.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"203.0.113.9","success":true,"country_code":"DE","region":"Hesse","city":"Frankfurt"}`))
	}))
	defer good.Close()

	saved := exitProbers
	defer func() { exitProbers = saved }()
	exitProbers = []exitProber{
		{url: silent.URL, decode: saved[0].decode},
		{url: good.URL, decode: saved[1].decode},
	}

	start := time.Now()
	ip, cc, _, city, ok := probeExitIPVia(context.Background(), (&net.Dialer{}).DialContext)
	if !ok || ip != "203.0.113.9" || cc != "DE" || city != "Frankfurt" {
		t.Fatalf("got ip=%q cc=%q city=%q ok=%v", ip, cc, city, ok)
	}
	if el := time.Since(start); el > exitProbeHedge+time.Second {
		t.Fatalf("took %v; waited out the silent prober", el)
	}
}

// A prober that answers with an error status hands over at once.
func TestProbeExitIPFailedProberHandsOverImmediately(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"203.0.113.10","country_code":"NL","region":"","city":"Amsterdam"}`))
	}))
	defer good.Close()

	saved := exitProbers
	defer func() { exitProbers = saved }()
	exitProbers = []exitProber{
		{url: limited.URL, decode: saved[0].decode},
		{url: good.URL, decode: saved[2].decode},
	}

	start := time.Now()
	ip, _, _, _, ok := probeExitIPVia(context.Background(), (&net.Dialer{}).DialContext)
	if !ok || ip != "203.0.113.10" {
		t.Fatalf("got ip=%q ok=%v", ip, ok)
	}
	if el := time.Since(start); el >= exitProbeHedge {
		t.Fatalf("took %v; a failed prober should hand over without the hedge delay", el)
	}
}
