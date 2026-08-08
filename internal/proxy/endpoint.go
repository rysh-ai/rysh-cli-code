// Package proxy implements the Universal Agent Governance Proxy (design 001):
// a loopback HTTP proxy that intercepts the LLM provider traffic of third-party
// agent CLIs (Claude Code, Codex, aider, …) running inside panes and applies
// rysh governance to it — SecretNAT redaction on request bodies, token/cost
// metering into the usage ledger (design 003), and audit — by base-URL
// environment injection, NOT TLS MITM.
package proxy

import "sync/atomic"

// endpoint holds the live loopback base URL (e.g. "http://127.0.0.1:53219") of
// the running proxy, or "" when no proxy is running. It is a process-global
// because the single session daemon runs one proxy and PaneActor reads it at
// shell-spawn time from deep in the actor hierarchy; threading it through every
// constructor would be far more invasive. Written once by the workspace when the
// server binds/unbinds; read by pane shell startup. atomic ⇒ race-free.
var endpoint atomic.Value // string

// current holds the running Server, for the same reason and with the same
// caveat as endpoint above: pane-side code (ungoverned-CLI detection, design
// 022 §4.4) needs to reach it from deep in the actor hierarchy. nil when no
// proxy is running, and every caller must handle that.
var current atomic.Value // *Server

// SetEndpoint records the running proxy's loopback base URL (or "" to clear).
func SetEndpoint(base string) { endpoint.Store(base) }

// Current returns the running proxy server, or nil when none is running.
func Current() *Server {
	s, _ := current.Load().(*Server)
	return s
}

// Endpoint returns the running proxy's base URL, or "" if none is running.
// PaneActor injects <endpoint>/<dialect>/<paneID> as ANTHROPIC_BASE_URL etc.
// only when this is non-empty, so a disabled proxy leaves pane env untouched.
func Endpoint() string {
	if v, ok := endpoint.Load().(string); ok {
		return v
	}
	return ""
}
