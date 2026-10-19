// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go"
)

// Design 028 §9 limit 7: a daemon whose port already answers joins that NATS
// server as a GUEST, and from then on keeps its session's state in that
// server's store. That is deliberate — it is how a daemon shares the desktop
// app sidecar's bus — but it used to be silent: from 2026-10-02 to 10-09 the
// live daemon was a guest of a stray `test` daemon, and every board post went
// into that daemon's store without a word. Guest mode stays. What changes is
// that it is visible: the guest records whose server it joined, asks the owning
// daemon who it is, and warns loudly when that is another session or project.
// Whether a mismatched host should be REFUSED is a founder question.

// HostIdentitySubject is answered by the daemon that OWNS an embedded NATS
// server, so a guest can tell whose store it is writing into.
const HostIdentitySubject = "rysh.bus.host.identity"

// defaultHostQueryTimeout bounds the guest's question. The query runs off the
// startup path, so this only decides how soon "unknown" is reported.
const defaultHostQueryTimeout = 300 * time.Millisecond

// unknownHost is what guest_of records for a host that did not identify itself.
const unknownHost = "unknown"

// HostIdentity is the owning daemon's answer on HostIdentitySubject.
type HostIdentity struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	PID       int    `json:"pid"`
}

// GuestOf describes the server a guest Bus joined. Its JSON field names are
// what /health and `##rysh status` show under "guest_of".
type GuestOf struct {
	ServerID      string `json:"server_id"`
	ServerName    string `json:"server_name,omitempty"`
	Port          int    `json:"port"`
	HostSession   string `json:"host_session"`             // "unknown" when the host did not answer
	HostWorkspace string `json:"host_workspace,omitempty"` // likewise; omitted from the public view
	HostPID       int    `json:"host_pid,omitempty"`
	UnknownReason string `json:"unknown_reason,omitempty"`
	Pending       bool   `json:"pending,omitempty"` // the host query is still in flight
	// Mismatch is the WARN-worthy case: the host belongs to another WORKSPACE
	// (ORDER-13, root-0273). Warning is set with it, and only with it.
	Mismatch bool `json:"mismatch"`
	// SessionMismatch / WorkspaceMismatch say exactly what differs. A host that
	// is another session of the SAME workspace (the desktop sidecar's daemon
	// under its own session name) is recorded in Note, at INFO, not warned.
	SessionMismatch   bool   `json:"session_mismatch"`
	WorkspaceMismatch bool   `json:"workspace_mismatch"`
	Note              string `json:"note,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

// Public is the subset safe for the unauthenticated /health endpoint: it drops
// the host's filesystem path and PID, and rewords the warning without either
// workspace path. `##rysh status` still shows all of it.
func (g GuestOf) Public() GuestOf {
	g.HostWorkspace = ""
	g.HostPID = 0
	if g.Mismatch {
		g.Warning = fmt.Sprintf("this daemon is a GUEST of NATS server %s on port %d, owned by session %q of another workspace: its state is being written into THAT daemon's store (details: ##rysh status)",
			g.ServerID, g.Port, g.HostSession)
	}
	return g
}

// GuestOf returns what this Bus knows about the server it joined as a guest;
// ok is false (and the value empty) when the Bus owns its server or is an
// external client.
func (b *Bus) GuestOf() (GuestOf, bool) {
	if !b.guest {
		return GuestOf{}, false
	}
	b.guestMu.Lock()
	defer b.guestMu.Unlock()
	return b.guestOf, true
}

func (b *Bus) logger() *slog.Logger {
	if b.log != nil {
		return b.log
	}
	return slog.Default()
}

// joinedAsGuest records the server just joined and starts the bounded host
// query in the background, so a slow or silent host never holds startup.
func (b *Bus) joinedAsGuest(cfg Config) {
	g := GuestOf{
		ServerID:   b.nc.ConnectedServerId(),
		ServerName: b.nc.ConnectedServerName(),
		Port:       b.clientPort,
		Pending:    true,
	}
	b.guestMu.Lock()
	b.guestOf = g
	b.guestMu.Unlock()
	b.logger().Info("bus: joined an existing NATS server as a GUEST — this session's state lives in that server's store",
		"server_id", g.ServerID, "server_name", g.ServerName, "port", g.Port, "session", cfg.SessionName)

	timeout := cfg.HostQueryTimeout
	if timeout <= 0 {
		timeout = defaultHostQueryTimeout
	}
	go b.askHost(cfg.SessionName, cfg.Workspace, timeout)
}

func (b *Bus) askHost(ourSession, ourWorkspace string, timeout time.Duration) {
	var id HostIdentity
	reason := ""
	reply, err := b.nc.Request(HostIdentitySubject, nil, timeout)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		reason = "no rysh daemon answered on " + HostIdentitySubject + " (the server's owner is not a rysh daemon, or predates this field)"
	case errors.Is(err, nats.ErrTimeout):
		reason = fmt.Sprintf("the host did not answer within %s", timeout)
	case err != nil:
		reason = "host query failed: " + err.Error()
	default:
		if jerr := json.Unmarshal(reply.Data, &id); jerr != nil || id.Session == "" {
			reason = "the host's answer was not a rysh host identity"
		}
	}

	b.guestMu.Lock()
	g := b.guestOf
	g.Pending = false
	if reason != "" {
		g.HostSession, g.HostWorkspace, g.UnknownReason = unknownHost, unknownHost, reason
	} else {
		g.HostSession, g.HostWorkspace, g.HostPID = id.Session, id.Workspace, id.PID
		sessionDiffers := id.Session != ourSession
		workspaceDiffers := ourWorkspace != "" && id.Workspace != "" && cleanPath(id.Workspace) != cleanPath(ourWorkspace)
		g.SessionMismatch, g.WorkspaceMismatch = sessionDiffers, workspaceDiffers
		switch {
		case workspaceDiffers:
			g.Mismatch = true
			g.Warning = fmt.Sprintf("this daemon (session %q, workspace %q) is a GUEST of NATS server %s on port %d, owned by session %q in workspace %q: its boards, layout and KV state are being written into THAT daemon's store",
				ourSession, ourWorkspace, g.ServerID, g.Port, id.Session, id.Workspace)
		case sessionDiffers:
			g.Note = fmt.Sprintf("the host is session %q of this same workspace (e.g. the desktop app's sidecar); this session's state lives in its store",
				id.Session)
		}
	}
	b.guestOf = g
	b.guestMu.Unlock()

	switch {
	case reason != "":
		b.logger().Info("bus: the NATS host did not identify itself; guest_of host recorded as unknown",
			"server_id", g.ServerID, "port", g.Port, "reason", reason)
	case g.Mismatch:
		b.logger().Warn("bus: !!! NATS GUEST OF ANOTHER WORKSPACE !!! "+g.Warning,
			"host_session", g.HostSession, "host_workspace", g.HostWorkspace, "host_pid", g.HostPID,
			"server_id", g.ServerID, "port", g.Port)
	case g.SessionMismatch:
		b.logger().Info("bus: NATS host identified: another session of this workspace; "+g.Note,
			"host_session", g.HostSession, "host_workspace", g.HostWorkspace, "host_pid", g.HostPID,
			"server_id", g.ServerID, "port", g.Port)
	default:
		b.logger().Info("bus: NATS host identified; it is this session's own daemon",
			"host_session", g.HostSession, "host_workspace", g.HostWorkspace, "host_pid", g.HostPID,
			"server_id", g.ServerID, "port", g.Port)
	}
}

// answerHostIdentity is run by the daemon that owns the embedded server.
func (b *Bus) answerHostIdentity(cfg Config) {
	body, _ := json.Marshal(HostIdentity{Session: cfg.SessionName, Workspace: cfg.Workspace, PID: os.Getpid()})
	if _, err := b.nc.Subscribe(HostIdentitySubject, func(m *nats.Msg) { _ = m.Respond(body) }); err != nil {
		b.logger().Warn("bus: could not answer host-identity queries; guests will see this server as unknown", "err", err)
	}
}

func cleanPath(p string) string { return filepath.Clean(p) }
