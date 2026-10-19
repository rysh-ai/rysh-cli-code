// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Every Recall bot of the process is served by one HTTP listener per loopback
// address, which hands each request to the bot whose public URL path it falls
// under (design 043 D9). An agent bot's public URL is
// <MEETING_PUBLIC_URL>/<agent>, and the tunnel forwards the whole prefix, so
// one listener and one tunnel carry any number of bots. Before D9 each bot
// bound its own listener, which capped a session at one Recall bot (D4).

type sharedListener struct {
	addr   string
	server *http.Server
	routes map[string]*Service // bot base path (public URL path, no trailing slash) -> bot
}

var listeners = struct {
	sync.Mutex
	byAddr map[string]*sharedListener
}{byAddr: map[string]*sharedListener{}}

// mountBot serves s under basePath on addr, binding addr if s is its first
// bot. The returned func unmounts s; the listener closes with its last bot.
func mountBot(addr, basePath string, s *Service) (func(), error) {
	listeners.Lock()
	defer listeners.Unlock()
	// Port 0 means "any free port": no tunnel can know it in advance, so
	// such a listener is never shared; it is keyed by the port it got.
	_, port, _ := net.SplitHostPort(addr)
	l := listeners.byAddr[addr]
	if port == "0" {
		l = nil
	}
	if l == nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, errors.New("cannot bind meeting listener; choose an unused loopback port")
		}
		if port == "0" {
			addr = ln.Addr().String()
		}
		l = &sharedListener{addr: addr, routes: map[string]*Service{}}
		l.server = &http.Server{Handler: l, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		listeners.byAddr[addr] = l
		go l.serve(ln)
	}
	if _, taken := l.routes[basePath]; taken {
		return nil, errors.New("the meeting listener already serves a bot at this public URL; give each bot its own path")
	}
	l.routes[basePath] = s
	return func() {
		listeners.Lock()
		defer listeners.Unlock()
		if l.routes[basePath] != s {
			return
		}
		delete(l.routes, basePath)
		if len(l.routes) == 0 && listeners.byAddr[addr] == l {
			delete(listeners.byAddr, addr)
			_ = l.server.Close()
		}
	}, nil
}

// serve runs the listener; if it stops on its own, every bot it served is
// paused with a notice, as a bot's own listener used to do.
func (l *sharedListener) serve(ln net.Listener) {
	err := l.server.Serve(ln)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return
	}
	listeners.Lock()
	var bots []*Service
	for _, s := range l.routes {
		bots = append(bots, s)
	}
	if listeners.byAddr[l.addr] == l {
		delete(listeners.byAddr, l.addr)
	}
	listeners.Unlock()
	for _, s := range bots {
		s.pause()
		s.mu.Lock()
		s.notice = "Meeting HTTP listener stopped"
		s.mu.Unlock()
	}
}

// ServeHTTP dispatches to the bot with the longest base path the request
// falls under; the bot matches its own routes exactly (Service.ServeHTTP).
func (l *sharedListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	listeners.Lock()
	var bot *Service
	best := -1
	for base, s := range l.routes {
		if strings.HasPrefix(r.URL.Path, base+"/") && len(base) > best {
			bot, best = s, len(base)
		}
	}
	listeners.Unlock()
	if bot == nil {
		http.NotFound(w, r)
		return
	}
	bot.ServeHTTP(w, r)
}
