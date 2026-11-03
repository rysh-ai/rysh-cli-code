// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Forge registration. Before a source arms the invoke responder for an API it
// registers the name with the server (request/reply on ws.<ws>.forge.register).
// The server binds the name to the key's user: the proxy stamps the user, the
// client never sends one. A name another member holds stays theirs, whether or
// not its record is active, so that member's register is refused; the same user
// re-registers freely (reconnects, several sessions). The source re-registers
// every forgeReregisterInterval and right after every reconnect, so its record
// stays active. An older server with no registry answers nothing (no responders
// or a timeout), and the source shares as it did before registration existed.

// forgeRegisterTimeout bounds one register/unregister request.
const forgeRegisterTimeout = 3 * time.Second

// forgeReregisterInterval is the heartbeat re-register period. A var so tests
// can shorten it.
var forgeReregisterInterval = 60 * time.Second

func forgeRegisterSubject(ws string) string   { return fmt.Sprintf("ws.%s.forge.register", ws) }
func forgeUnregisterSubject(ws string) string { return fmt.Sprintf("ws.%s.forge.unregister", ws) }

// forgeRegisterReq is the register/unregister body (v1). Ops is informational
// (names only) and is left out of unregister.
type forgeRegisterReq struct {
	V      int      `json:"v"`
	API    string   `json:"api"`
	Source string   `json:"source"`
	Ops    []string `json:"ops,omitempty"`
}

type forgeRegisterResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type forgeRegOutcome int

const (
	forgeRegOK         forgeRegOutcome = iota // the server bound (or re-activated) the name
	forgeRegNoRegistry                        // no reply: an older server; share as before
	forgeRegRefused                           // the server said no; reason given
	forgeRegFailed                            // transport or a malformed reply
)

// validForgeAPIName reports whether name can be an API name on the wire: it is
// one token of forge.api.<api>.invoke, so it must be non-empty and hold no
// '.', '*', '>' or whitespace.
func validForgeAPIName(name string) bool {
	return name != "" && !strings.ContainsAny(name, ".*> \t\r\n")
}

// forgeAPINameRule is the user-facing statement of validForgeAPIName.
const forgeAPINameRule = "an API name cannot be empty or contain '.', '*', '>' or spaces"

// registerAPI sends one register request for name on nc. It is safe off the
// actor mailbox (the heartbeat and reconnect paths call it from goroutines).
func (a *ForgeShareActor) registerAPI(nc *nats.Conn, name string) (forgeRegOutcome, string) {
	var ops []string
	if a.forge != nil {
		for _, op := range a.forge.APIOps(name) {
			ops = append(ops, op.Name)
		}
	}
	body, _ := json.Marshal(forgeRegisterReq{V: 1, API: name, Source: a.uid, Ops: ops})
	return forgeRequest(nc, forgeRegisterSubject(a.ws), body)
}

// unregisterAPI tells the server this source no longer shares name (best
// effort; an older server ignores it).
func (a *ForgeShareActor) unregisterAPI(nc *nats.Conn, name string) (forgeRegOutcome, string) {
	body, _ := json.Marshal(forgeRegisterReq{V: 1, API: name, Source: a.uid})
	return forgeRequest(nc, forgeUnregisterSubject(a.ws), body)
}

func forgeRequest(nc *nats.Conn, subject string, body []byte) (forgeRegOutcome, string) {
	if nc == nil {
		return forgeRegFailed, "not connected"
	}
	reply, err := nc.Request(subject, body, forgeRegisterTimeout)
	switch {
	case errors.Is(err, nats.ErrNoResponders), errors.Is(err, nats.ErrTimeout):
		return forgeRegNoRegistry, ""
	case err != nil:
		return forgeRegFailed, err.Error()
	}
	var resp forgeRegisterResp
	if json.Unmarshal(reply.Data, &resp) != nil {
		return forgeRegFailed, "the server's reply is not a registration reply"
	}
	if !resp.OK {
		reason := resp.Error
		if reason == "" {
			reason = "no reason given"
		}
		return forgeRegRefused, reason
	}
	return forgeRegOK, ""
}

// reregisterShared re-registers every API this source shares, on nc. Runs off
// the mailbox (heartbeat ticker, reconnect). A refusal here means the name was
// cleared by a workspace admin and is now held by someone else: the server no
// longer serves this source for it, so it is logged for the owner to see.
func (a *ForgeShareActor) reregisterShared(nc *nats.Conn) {
	a.mu.Lock()
	names := make([]string, 0, len(a.shared))
	for n := range a.shared {
		names = append(names, n)
	}
	a.mu.Unlock()
	for _, n := range names {
		if out, reason := a.registerAPI(nc, n); out == forgeRegRefused || out == forgeRegFailed {
			slog.Warn("forge-share: re-register failed", "api", n, "outcome", out, "reason", reason)
		}
	}
}

// startReregister starts the heartbeat re-register loop (mailbox only). It is a
// no-op while one is running.
func (a *ForgeShareActor) startReregister() {
	if a.reregStop != nil || a.nc == nil {
		return
	}
	stop := make(chan struct{})
	a.reregStop = stop
	nc := a.nc
	go func() {
		t := time.NewTicker(forgeReregisterInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.reregisterShared(nc)
			case <-stop:
				return
			}
		}
	}()
}

// stopReregister stops the heartbeat loop (mailbox only).
func (a *ForgeShareActor) stopReregister() {
	if a.reregStop != nil {
		close(a.reregStop)
		a.reregStop = nil
	}
}
