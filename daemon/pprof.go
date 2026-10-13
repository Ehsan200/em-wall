package main

import (
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// startPprof serves Go's profiler on addr. Off unless -pprof is given;
// refused on anything but loopback, since profiles expose heap contents
// of a root process. Use: go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
func startPprof(addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("em-walld: pprof: bad address %q: %v", addr, err)
		return
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		log.Printf("em-walld: pprof: refusing non-loopback address %q", addr)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("em-walld: pprof listening on http://%s/debug/pprof/", addr)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("em-walld: pprof: %v", err)
		}
	}()
}
