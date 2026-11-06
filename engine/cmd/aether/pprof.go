package main

import (
	"log"
	"net"
	"net/http"
	"net/http/pprof"
)

// servePprof serves Go's profiler, for finding where a load test's CPU and
// memory went (ARCHITECTURE 6bu). Loopback only: a profile shows what the
// process is doing, which is not for the network to read.
func servePprof(addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatalf("-pprof must be a loopback address, got %q", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/heap", pprof.Index)
	go func() { log.Println(http.ListenAndServe(addr, mux)) }()
}
