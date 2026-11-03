// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The fleet board in the board view (design 030 §4.3 and §9.2, wave W4).
//
// The reserved `fleet` board opens on a DASHBOARD: one row per registered
// fleet from the daemon's FleetBoardActor (latest wins), not a scroll. It is
// READ-ONLY: no composer (i / enter say so), no board claude, no pane roster.
// The raw post stream, with `(unverified)` posts marked, stays one key away
// (s p a f t) for audit; d returns to the dashboard.
//
// The rows come from fleetboard.Ask, asked once at start and again each time
// the actor publishes on msg.FleetBoardUpdateSubject. Health is recomputed at
// draw time, so a fleet turns quiet on screen without a new update.

// fleetBoardMsg carries one fleet-board answer into Update.
type fleetBoardMsg struct{ reply *fleetboard.Reply }

const fleetBoardAskTimeout = 1500 * time.Millisecond

// setupFleetBoardSubscription asks for the rows now and after every update.
func setupFleetBoardSubscription(b *bus.Bus) chan fleetBoardMsg {
	ch := make(chan fleetBoardMsg, 4)
	if b == nil || b.Conn() == nil {
		return ch
	}
	nc := b.Conn()
	ask := func() {
		if reply, err := fleetboard.Ask(nc, fleetBoardAskTimeout); err == nil {
			select {
			case ch <- fleetBoardMsg{reply: reply}:
			default: // the next answer carries the same and more
			}
		}
	}
	go ask()
	_, _ = nc.Subscribe(msgpkg.FleetBoardUpdateSubject(), func(*nats.Msg) { ask() })
	return ch
}

// listenFleetBoardCmd blocks until the next fleet-board answer.
func (m Model) listenFleetBoardCmd() tea.Cmd {
	ch := m.fleetBoardCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return <-ch }
}

// isFleetBoardPane reports whether a board pane shows the reserved fleet board.
func (m Model) isFleetBoardPane(paneID string) bool {
	return m.boardIDForPane(paneID) == msgpkg.FleetBoardID
}

// fleetBoardReadOnlyNote is what i / enter say on the fleet board.
const fleetBoardReadOnlyNote = "the fleet board is read-only: one row per fleet. Fleet heads post with " +
	"rysh board post --board fleet --as <pane> -- '<line>'"

// boardFleetDashLayout draws the dashboard.
func (m Model) boardFleetDashLayout(pane domain.PaneSnapshot, st *boardViewState, contentWidth, maxLines int) boardLayout {
	now := time.Now().UnixMilli()
	var rows []string
	n := 0
	if m.fleetBoard != nil {
		n = len(m.fleetBoard.Rows)
		for _, r := range m.fleetBoard.Rows {
			r.Health = r.Row.Health(now) // fresh at draw time
			rows = append(rows, fleetboard.RenderLine(r, now))
		}
	}
	if n == 0 {
		rows = []string{
			"no fleet rows yet: no fleet is registered, or this session's daemon has no fleet board",
			"(an older build). s shows the raw post stream.",
		}
	}
	st.lastRows = len(rows)
	st.lastVisible = maxLines
	st.scrollOffset = clampBoardScroll(st.scrollOffset, st)
	start, end := visibleRowRange(len(rows), maxLines, st.scrollOffset)
	lay := boardLayout{rows: make([]string, 0, maxLines), start: start}
	for i := start; i < end; i++ {
		lay.rows = append(lay.rows, truncBoardLine(rows[i], contentWidth))
	}
	for len(lay.rows) < maxLines {
		lay.rows = append(lay.rows, "")
	}
	lay.refs = make([]boardRowRef, len(lay.rows))
	lay.all = lay.rows
	lay.allRefs = lay.refs
	lay.header = truncBoardLine(fmt.Sprintf("FLEET BOARD | %d fleets · read-only · d dashboard  s stream", n),
		contentWidth)
	lay.input = ""
	status := "read-only · " + "fleet heads post with: rysh board post --board fleet --as <pane> -- '<line>'"
	if st.sendStatus != "" {
		status = m.boardStatusLine(st)
	}
	lay.status = truncBoardLine(status, contentWidth)
	return lay
}
