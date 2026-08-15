// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Pane attribution is the proxy's primary key (design 001 §3): every control —
// ceilings, request rates, tenant pins, audit — is keyed on the {paneID}
// segment of /{dialect}/{paneID}/{rest...}.
//
// That segment arrives from INSIDE the pane, where a wrapped CLI (or anything
// else the user runs) chooses it freely. Two concrete escapes followed:
//
//   - An EMPTY segment. `POST /anthropic//v1/messages` reached the upstream with
//     no budget check (budget.go returns allowed for an empty pane) and no
//     per-pane bucket (ratelimit.go skips it), so a governed pane could opt out
//     of both by deleting one path component.
//   - A FOREIGN segment. Naming another pane inherits that pane's tenant pin,
//     which is exactly what "a pin always beats the header" (022 §4.3) rests on.
//     Without validation the pin is selectable by the thing it caps.
//
// So the proxy keeps its own set of panes rysh actually injected an endpoint
// into, and refuses anything else.

// maxPaneIDLen bounds the segment. Pane IDs are short rysh-minted identifiers;
// anything long is a caller doing something else with the field.
const maxPaneIDLen = 64

// paneRegistry is the set of live panes, maintained by the pane lifecycle:
// NotePane at shell spawn (where the base URL is injected), ForgetPane at exit.
//
// The mutex guards only this small set — the same documented data-plane
// exception as the audit ring, the budget cache and the rate limiter.
type paneRegistry struct {
	mu    sync.Mutex
	known map[string]struct{}
	// armed latches on the first registration and never clears. Without it,
	// closing the last pane would empty the set and re-open the "nobody has
	// registered anything" allowance below — so a daemon would go from
	// enforcing to permissive at the exact moment it has no live panes.
	armed bool
}

func newPaneRegistry() *paneRegistry {
	return &paneRegistry{known: map[string]struct{}{}}
}

func (p *paneRegistry) note(paneID string) {
	if p == nil || paneID == "" {
		return
	}
	p.mu.Lock()
	p.known[paneID] = struct{}{}
	p.armed = true
	p.mu.Unlock()
}

func (p *paneRegistry) forget(paneID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.known, paneID)
	p.mu.Unlock()
}

// allows reports whether paneID may be used.
//
// A registry that has NEVER seen a registration allows any syntactically valid
// pane. That is not a hole, it is the honest answer for a proxy nobody has
// registered a pane with: the probe proxy behind `##proxy check`,
// cmd/wire-harness, and every test construct a Server directly and never run a
// pane lifecycle. Enforcing against an empty set there would refuse all
// traffic. From the first registration onward the registry is authoritative —
// including after the last pane closes, which is why `armed` latches.
func (p *paneRegistry) allows(paneID string) bool {
	if !validPaneID(paneID) {
		return false
	}
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.armed {
		return true
	}
	_, ok := p.known[paneID]
	return ok
}

// validPaneID is the syntactic gate, applied even with an empty registry. It is
// deliberately narrower than "not empty": the ID also names a file in the
// audit_content sink and a NATS subject token, so a path separator or a wildcard
// here is a problem elsewhere.
func validPaneID(paneID string) bool {
	if paneID == "" || len(paneID) > maxPaneIDLen {
		return false
	}
	for _, r := range paneID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	// "." and ".." would traverse in any path context that ever consumes this.
	return strings.Trim(paneID, ".") != ""
}

// NotePane registers a pane as a legitimate attribution target. Called where
// rysh injects the pane's base URL, so the set of panes the proxy accepts is
// exactly the set it pointed at itself.
func (s *Server) NotePane(paneID string) { s.panes.note(paneID) }

// writeUnknownPane refuses a request whose pane segment is empty, malformed or
// not a live pane.
//
// 403 with a provider-shaped body, not a bare text error: the caller is a
// wrapped CLI, and an unparseable body turns a clean refusal into a crash. It is
// recorded in the audit ring because an attempt to spend under someone else's
// pane ID is exactly the kind of thing an operator should be able to see after
// the fact — but nothing is published to the pane's own output, since the pane
// named is not one we trust.
func (s *Server) writeUnknownPane(w http.ResponseWriter, d Dialect, paneID string) {
	msgText := "rysh governance proxy: unknown pane in the request path. " +
		"Provider traffic must be addressed as /{dialect}/{paneID}/… with the " +
		"pane ID rysh injected into this shell's base URL."
	writeDialectError(w, d, http.StatusForbidden, msgText)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: d.Name(),
		Endpoint: "(unknown-pane)", Status: http.StatusForbidden,
	}
	s.recordAudit(audit)
	slog.Warn("proxy: request refused — unknown pane in the path",
		"pane", paneID, "dialect", d.Name())

	// The durable record is published only for a pane ID that is at least
	// well-formed. The subject carries the pane ID as a token, and a
	// syntactically invalid ID is exactly the input that must never be allowed
	// to shape a subject.
	if validPaneID(paneID) {
		s.publishRefusalAudit(audit, msg.ProxyBlocked)
	}
}
