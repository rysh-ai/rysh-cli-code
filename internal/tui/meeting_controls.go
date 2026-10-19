// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Meeting-bot lifecycle controls (E17 h, MTG-6, design 040). A meeting pane
// (design 043) shows E30's placeholder; under it this panel shows the bot's
// turn state and answers keys with the design-040 lifecycle verbs the TUI
// lacked — pending, approve/reject, speak, pause/resume, cancel. Each is the
// SAME daemon request the CLI sends (MsgMeetingCommand to ws.inbox), always
// addressed to the pane's own meeting id in the daemon's registry (E30 PM-1),
// so with several meetings open a key on one pane can never act on another.
// list, status and leave stay with ##meeting (PM-1/PM-4).
//
// This file is the whole panel; meeting_view.go (E30's) is not touched — the
// view composes the two at the meeting-pane call site.

const (
	// meetingPollInterval is how often the focused meeting pane re-reads its
	// pending turn, so an approval request shows up without a keypress.
	meetingPollInterval = 3 * time.Second
	// meetingRequestTimeout matches `rysh meeting`'s own request timeout.
	meetingRequestTimeout = 30 * time.Second
)

// meetingCtl is one meeting pane's panel state.
type meetingCtl struct {
	reply    *meeting.Reply // last pending reply: state, phase, the turn waiting
	note     string         // outcome of the last action
	inflight string         // action awaiting the daemon's answer ("" = none)
}

// meetingReplyMsg carries the daemon's answer to one panel request.
type meetingReplyMsg struct {
	paneID, action string
	reply          meeting.Reply
	err            string
}

// meetingPollTick re-reads the focused meeting pane's pending turn.
type meetingPollTick struct{ paneID string }

// meetingCtlFor returns (creating if needed) a meeting pane's panel state.
func (m *Model) meetingCtlFor(paneID string) *meetingCtl {
	if m.meetingControls == nil {
		m.meetingControls = make(map[string]*meetingCtl)
	}
	ctl := m.meetingControls[paneID]
	if ctl == nil {
		ctl = &meetingCtl{}
		m.meetingControls[paneID] = ctl
	}
	return ctl
}

// meetingCommandFor builds the daemon command for action on a meeting pane.
// ok=false when it cannot be sent: the pane has no registry id (the daemon's
// "" default means "the only meeting" — exactly the cross-talk to avoid), or
// a turn verb has no turn to name.
func meetingCommandFor(pane domain.PaneSnapshot, action string, ctl *meetingCtl) (meeting.Command, bool) {
	id := pane.Meta["meeting.id"]
	if !meeting.ValidMeetingID(id) {
		return meeting.Command{}, false
	}
	c := meeting.Command{Action: action, ID: id}
	switch action {
	case "approve", "reject", "speak":
		if ctl == nil || ctl.reply == nil || ctl.reply.TurnID == "" {
			return c, false
		}
		c.TurnID = ctl.reply.TurnID
	}
	return c, true
}

// meetingRequestCmd sends one command and yields the daemon's answer.
func (m Model) meetingRequestCmd(paneID string, c meeting.Command) tea.Cmd {
	pub, inbox := m.pub, m.workspaceInbox
	return func() tea.Msg {
		if pub == nil {
			return meetingReplyMsg{paneID: paneID, action: c.Action, err: "not connected to the daemon"}
		}
		raw, err := pub.Request(inbox, &msgpkg.MsgMeetingCommand{Command: c}, meetingRequestTimeout)
		if err != nil {
			return meetingReplyMsg{paneID: paneID, action: c.Action, err: "no answer from the daemon"}
		}
		r, ok := raw.(*msgpkg.MsgMeetingReply)
		if !ok {
			return meetingReplyMsg{paneID: paneID, action: c.Action, err: "unexpected reply; update and restart the daemon"}
		}
		return meetingReplyMsg{paneID: paneID, action: c.Action, reply: r.Reply, err: r.Reply.Error}
	}
}

// meetingRefreshCmd re-reads a meeting pane's pending turn.
func (m *Model) meetingRefreshCmd(paneID string) tea.Cmd {
	pane := m.findPaneInSnapshot(paneID)
	if pane == nil {
		return nil
	}
	c, ok := meetingCommandFor(*pane, "pending", nil)
	if !ok {
		return nil
	}
	return m.meetingRequestCmd(paneID, c)
}

// updateMeetingPaneInput handles keys while the focused pane is a meeting
// pane. Only the control keys and enter are claimed; esc, chords and
// everything else pass through, so the user is never trapped.
func (m *Model) updateMeetingPaneInput(msg tea.KeyMsg) (bool, tea.Cmd) {
	snap := m.focusedPaneSnapshot()
	if snap == nil || snap.PaneType != domain.PaneTypeMeeting {
		return false, nil
	}
	ctl := m.meetingCtlFor(snap.ID)
	state := ""
	if ctl.reply != nil {
		state = ctl.reply.State
	}
	send := func(action string) (bool, tea.Cmd) {
		c, ok := meetingCommandFor(*snap, action, ctl)
		if !ok {
			if !meeting.ValidMeetingID(snap.Meta["meeting.id"]) {
				ctl.note = "this pane has no meeting id; nothing sent"
			} else {
				ctl.note = "no turn to " + action
			}
			return true, nil
		}
		ctl.inflight = action
		ctl.note = action + "…"
		return true, m.meetingRequestCmd(snap.ID, c)
	}
	refuse := func(note string) (bool, tea.Cmd) {
		ctl.note = note
		return true, nil
	}
	switch msg.String() {
	case "g":
		return send("pending")
	case "a":
		if state != "awaiting_approval" {
			return refuse("no turn awaiting approval")
		}
		return send("approve")
	case "x":
		if state != "awaiting_approval" && state != "awaiting_speech" {
			return refuse("no turn waiting to reject")
		}
		return send("reject")
	case "s":
		if state != "awaiting_speech" {
			return refuse("no turn awaiting speech")
		}
		return send("speak")
	case "p":
		if state == "paused" {
			return send("resume")
		}
		return send("pause")
	case "c":
		return send("cancel")
	case "enter":
		// A meeting pane has no shell; enter would only print an error.
		return true, nil
	}
	return false, nil
}

// applyMeetingReply folds the daemon's answer into the pane's panel. A
// pending answer is the panel's picture of the bot; any action's answer is
// noted and followed by a pending refresh so the picture catches up.
func (m *Model) applyMeetingReply(r meetingReplyMsg) tea.Cmd {
	ctl := m.meetingCtlFor(r.paneID)
	ctl.inflight = ""
	if r.action == "pending" {
		if r.err != "" {
			ctl.note = "pending: " + r.err
			return nil
		}
		rep := r.reply
		ctl.reply = &rep
		return nil
	}
	if r.err != "" {
		ctl.note = r.action + " failed: " + r.err
	} else {
		ctl.note = r.action + ": " + r.reply.State
		if r.reply.Notice != "" {
			ctl.note += " — " + r.reply.Notice
		}
	}
	return m.meetingRefreshCmd(r.paneID)
}

// meetingPollStart starts the pending-turn poll when a meeting pane has focus
// (called on every applied workspace snapshot) and drops panel state of panes
// that are gone. One poll chain at a time.
func (m *Model) meetingPollStart() tea.Cmd {
	for id := range m.meetingControls {
		if m.findPaneInSnapshot(id) == nil {
			delete(m.meetingControls, id)
		}
	}
	snap := m.focusedPaneSnapshot()
	if m.meetingPolling || snap == nil || snap.PaneType != domain.PaneTypeMeeting {
		return nil
	}
	m.meetingPolling = true
	return tea.Batch(m.meetingRefreshCmd(snap.ID), meetingPollTickCmd(snap.ID))
}

func meetingPollTickCmd(paneID string) tea.Cmd {
	return tea.Tick(meetingPollInterval, func(time.Time) tea.Msg { return meetingPollTick{paneID: paneID} })
}

// onMeetingPollTick continues the chain on whichever meeting pane now has
// focus, and ends it when none does.
func (m *Model) onMeetingPollTick() tea.Cmd {
	snap := m.focusedPaneSnapshot()
	if snap == nil || snap.PaneType != domain.PaneTypeMeeting {
		m.meetingPolling = false
		return nil
	}
	return tea.Batch(m.meetingRefreshCmd(snap.ID), meetingPollTickCmd(snap.ID))
}

// meetingControlLines renders the panel for a meeting pane.
func meetingControlLines(ctl *meetingCtl) []string {
	if ctl == nil || ctl.reply == nil {
		lines := []string{"bot: waiting for status…  (g refresh)"}
		if ctl != nil && ctl.note != "" {
			lines = append(lines, ctl.note)
		}
		return lines
	}
	r := ctl.reply
	head := "bot: " + r.State
	if r.Phase != "" && r.Phase != r.State {
		head += " · " + r.Phase
	}
	if r.Mode != "" {
		head += " · " + r.Mode
	}
	lines := []string{head}
	if r.TurnID != "" {
		who := r.Agent
		if who == "" {
			who = "rysh"
		}
		turn := fmt.Sprintf("turn %s", r.TurnID)
		if r.Speaker != "" {
			turn += " · " + r.Speaker + " → " + who
		}
		lines = append(lines, turn)
		if r.Request != "" {
			lines = append(lines, "  request: "+strings.Join(strings.Fields(r.Request), " "))
		}
		if r.Result != "" {
			lines = append(lines, "  reply: "+strings.Join(strings.Fields(r.Result), " "))
		}
	}
	if ctl.note != "" {
		lines = append(lines, ctl.note)
	}
	var keys []string
	switch r.State {
	case "awaiting_approval":
		keys = append(keys, "a approve", "x reject")
	case "awaiting_speech":
		keys = append(keys, "s speak", "x reject")
	}
	if r.State == "paused" {
		keys = append(keys, "p resume")
	} else {
		keys = append(keys, "p pause")
	}
	keys = append(keys, "c cancel", "g refresh")
	return append(lines, strings.Join(keys, " · "))
}

// buildMeetingPanelWithControls is the meeting pane's view: E30's placeholder
// (buildMeetingPanel, unmodified) on top, this panel at the bottom, exactly
// height lines. A pane too short for both shows the placeholder alone.
func (m Model) buildMeetingPanelWithControls(pane domain.PaneSnapshot, paneWidth, height int) string {
	lines := meetingControlLines(m.meetingControls[pane.ID])
	top := height - len(lines) - 1
	if top < 2 {
		return m.buildMeetingPanel(pane, paneWidth, height)
	}
	width := max(8, paneWidth-4)
	out := []string{m.buildMeetingPanel(pane, paneWidth, top), ""}
	for _, l := range lines {
		out = append(out, ansi.Truncate(l, width, "…"))
	}
	return strings.Join(out, "\n")
}
