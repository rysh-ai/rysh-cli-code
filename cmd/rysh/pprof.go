// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
)

// startPprof serves net/http/pprof on RYSH_PPROF (e.g. "127.0.0.1:6060") when
// it is set, and does nothing otherwise. It exists because a daemon that has
// grown to gigabytes cannot be diagnosed after the fact: Go heap shows up in
// `footprint` as one opaque "untagged (VM_ALLOCATE)" region, and the only way
// to attribute it is a heap profile taken from inside the process.
//
// Loopback only — a non-loopback host is refused, because pprof exposes
// goroutine stacks and heap contents.
func startPprof(logger *slog.Logger) {
	addr := os.Getenv("RYSH_PPROF")
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		logger.Warn("rysh: RYSH_PPROF must be a loopback host:port; pprof disabled", "addr", addr)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Warn("rysh: pprof listen failed", "addr", addr, "err", err)
		return
	}
	logger.Info("rysh: pprof serving", "addr", ln.Addr().String())
	go func() { _ = http.Serve(ln, mux) }()
}
