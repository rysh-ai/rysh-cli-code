// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
)

// Design 028 §9 limit 7: a daemon whose port already answered joined that NATS
// server as a GUEST, and keeps its session's state in the owner's store. It
// used to be silent (10-02…10-09 the live daemon wrote into a stray `test`
// daemon's store); `##rysh status` and /health now say whose server it is.

// sessionGuestOf is the daemon Bus's guest_of; ok is false for a daemon that
// owns its server, and when nothing is wired (tests, attach-only builds).
func (w *WorkspaceActor) sessionGuestOf() (bus.GuestOf, bool) {
	if w.guestOf == nil {
		return bus.GuestOf{}, false
	}
	return w.guestOf()
}

// wireWebGuestOf gives a just-created web server its /health guest_of. Called
// right after each web.NewServer, before Start.
func (w *WorkspaceActor) wireWebGuestOf() {
	if w.webServer == nil {
		return
	}
	w.webServer.SetGuestOf(func() any {
		if g, ok := w.sessionGuestOf(); ok {
			return g.Public()
		}
		return nil
	})
}

// handleRyshStatus implements `##rysh status [--json]`.
func (w *WorkspaceActor) handleRyshStatus(out *strings.Builder, asJSON bool) {
	g, guest := w.sessionGuestOf()
	if asJSON {
		v := struct {
			Session string       `json:"session"`
			GuestOf *bus.GuestOf `json:"guest_of"`
		}{Session: w.sessionName}
		if guest {
			v.GuestOf = &g
		}
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteString("\n")
		return
	}
	fmt.Fprintf(out, "\n[rysh] status\n  session   %s\n", w.sessionName)
	if !guest {
		out.WriteString("  nats      this daemon owns its NATS server (guest_of: none)\n")
		return
	}
	fmt.Fprintf(out, "  nats      guest of NATS server %s on port %d", g.ServerID, g.Port)
	if g.ServerName != "" && g.ServerName != g.ServerID {
		fmt.Fprintf(out, " (%s)", g.ServerName)
	}
	out.WriteString(" — this session's boards, layout and KV state live in THAT server's store\n")
	switch {
	case g.Pending:
		out.WriteString("  host      asking the host who it is…\n")
	case g.UnknownReason != "":
		fmt.Fprintf(out, "  host      unknown: %s\n", g.UnknownReason)
	default:
		fmt.Fprintf(out, "  host      session %s, workspace %s", g.HostSession, g.HostWorkspace)
		if g.HostPID > 0 {
			fmt.Fprintf(out, ", pid %d", g.HostPID)
		}
		out.WriteString("\n")
	}
	switch {
	case g.Mismatch:
		fmt.Fprintf(out, "  WARNING   %s\n", g.Warning)
	case g.Note != "":
		fmt.Fprintf(out, "  NOTE      %s\n", g.Note)
	}
}
