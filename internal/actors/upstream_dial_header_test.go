// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// recordingProxy stands in for rysh-server's transparent NATS proxy: it
// records each WebSocket upgrade it forwards to a real NATS WebSocket
// listener.
type recordingProxy struct {
	mu       sync.Mutex
	requests []*http.Request
}

func startNATSBehindRecordingProxy(t *testing.T) (*recordingProxy, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wsPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	opts.Websocket.Host, opts.Websocket.Port, opts.Websocket.NoTLS = "127.0.0.1", wsPort, true
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats did not start")
	}
	rec := &recordingProxy{}
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(wsPort))
	forward := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Clone(r.Context()))
		rec.mu.Unlock()
		r.URL.Path, r.URL.RawQuery = "/", "" // NATS listens at the root
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return rec, srv.URL
}

// F-86: the upstream NATS dial sends the workspace API key in the X-API-Key
// upgrade header, never as ?api_key= in the URL — URLs are what access logs
// record. rysh-server has accepted X-API-Key on /nats since its first commit.
func TestUpstreamDialSendsTheAPIKeyInAHeaderNotTheURL(t *testing.T) {
	rec, serverURL := startNATSBehindRecordingProxy(t)
	nc, err := dialUpstream(config.UpstreamConfig{URL: serverURL, Workspace: "ws-1", APIKey: "rysh_secret_key"}, "dial-test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer nc.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) == 0 {
		t.Fatal("no upgrade reached the proxy")
	}
	req := rec.requests[0]
	if req.URL.Path != "/workspaces/ws-1/nats" || req.URL.Query().Get("connection_type") != "share" {
		t.Fatalf("upgrade went to %s, want /workspaces/ws-1/nats?connection_type=share", req.URL.String())
	}
	if req.URL.Query().Has("api_key") {
		t.Fatalf("the API key is in the URL: %s", req.URL.String())
	}
	if got := req.Header.Get("X-API-Key"); got != "rysh_secret_key" {
		t.Fatalf("X-API-Key header = %q, want the workspace key", got)
	}
}
