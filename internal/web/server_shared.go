package web

// The SHARED door.
//
// A session's daemon runs exactly one web server: one route set, one hub, one
// view of the session. The desktop app is its first client — it spawns the
// daemon and talks to it over a loopback port with no credential, which is
// safe precisely because that port is loopback and the app is the process that
// started it.
//
// Handing that same UI to a browser needs the opposite posture: a password, and
// possibly a bind reachable from a phone. The two cannot both be true of one
// listener, so the server grows a SECOND listener onto the same routes. Which
// door a request arrived through decides whether the login is enforced:
//
//	private listener (the app's)  → ungated in control mode
//	shared  listener (this file)  → always requires the login
//
// That is one web server with two doors, not two servers: a pane opened in the
// app and the same pane watched in the browser are the same pane, because both
// requests land on the same handler over the same hub.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
)

// gatedCtxKey marks a request as having arrived on the shared listener.
type gatedCtxKey struct{}

// arrivedShared reports whether this request came through the shared door.
func arrivedShared(r *http.Request) bool {
	v, _ := r.Context().Value(gatedCtxKey{}).(bool)
	return v
}

// SetShared configures the shared door to be opened at Start. Host empty means
// DefaultHost. Use StartShared to open one on a server already running.
func (s *Server) SetShared(host string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sharedHost, s.sharedPort = host, port
}

// SharedAddr returns the shared door's host and port, or ("", 0) when closed.
func (s *Server) SharedAddr() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sharedLn == nil {
		return "", 0
	}
	return s.sharedHost, s.sharedPort
}

// StartShared opens the shared door on host:port. The server must be running,
// and credentials must be configured: the whole point of this listener is that
// it demands them, so opening one that could not check a password would serve
// the session to anyone who found the port.
//
// Opening a second shared door closes the first — one shared address at a time,
// so there is never an old port still serving after a rebind.
func (s *Server) StartShared(host string, port int) error {
	s.mu.Lock()
	engine, running := s.engine, s.running
	s.mu.Unlock()

	if !running || engine == nil {
		return fmt.Errorf("web server is not running")
	}
	if s.credentials() == nil {
		return fmt.Errorf("no web login configured — a shared address is never served without one")
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("port %d out of range (1-65535)", port)
	}
	if host == "" {
		host = DefaultHost
	}

	// Every request through this listener is marked, and authMiddleware turns
	// that mark into "a login is required" regardless of control mode.
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), gatedCtxKey{}, true)))
	})

	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen on %s: %w", net.JoinHostPort(host, strconv.Itoa(port)), err)
	}

	_ = s.StopShared()

	srv := &http.Server{Handler: gated}
	s.mu.Lock()
	s.sharedSrv, s.sharedLn = srv, ln
	s.sharedHost, s.sharedPort = host, port
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("shared web listener error", "err", err)
		}
	}()
	return nil
}

// StopShared closes the shared door, leaving the private one (and every client
// on it) untouched. A no-op when none is open.
func (s *Server) StopShared() error {
	s.mu.Lock()
	srv := s.sharedSrv
	s.sharedSrv, s.sharedLn = nil, nil
	s.sharedHost, s.sharedPort = "", 0
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	return srv.Close()
}
