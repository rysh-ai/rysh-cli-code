// SPDX-License-Identifier: Apache-2.0

package board

// Board views: STATUS messages vs PROMPT messages (design 051).
//
// A board carries two kinds of traffic since design 050:
//
//   - status: what an agent says about itself — milestones, blockers, DONE.
//     Nobody consumes a status post as work.
//   - prompt: what one agent tells another to DO — a `cmd` post (rysh board
//     send, fleetctl msg/report under queue/grid messaging), together with the
//     thread it opens: the recipient's ack, replies on it, and the courier's or
//     listener's notices.
//
// The unit is the THREAD, not the post: an ack shown without the order it
// acknowledges, or an order shown without its ack, is half a conversation. So
// a prompt thread is kept or dropped whole.

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// View selects which threads a reader sees.
type View string

const (
	ViewStatus View = "status"
	ViewPrompt View = "prompt"
	ViewAll    View = "all"

	// ViewFlow is not a filter but a different rendering of the whole board:
	// who orders whom, and in which order (design 052, flow.go). As a filter
	// it keeps every thread.
	ViewFlow View = "flow"

	// ViewTeam is the fleet status table and the dependency tree (team.go):
	// who is progressing, idle or blocked, and on whom. Like flow, not a
	// filter: as one it keeps every thread.
	ViewTeam View = "team"
)

// DefaultView is what a board pane opens on: the agents' own status, the
// stream a human watches to know how the fleet is doing.
const DefaultView = ViewStatus

// ParseView accepts status|prompt|all|flow|team (and the one-letter keys s|p|a|f|t).
func ParseView(s string) (View, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "status", "s":
		return ViewStatus, nil
	case "prompt", "prompts", "p":
		return ViewPrompt, nil
	case "all", "a":
		return ViewAll, nil
	case "flow", "f":
		return ViewFlow, nil
	case "team", "t":
		return ViewTeam, nil
	}
	return "", fmt.Errorf("unknown board view %q: want status, prompt, all, flow or team", s)
}

// IsPromptThread reports whether a thread is a prompt conversation: its root is
// a command. A provisional thread (its root not arrived yet) is judged by what
// it holds: any command or ack in it makes it a prompt thread.
func IsPromptThread(t Thread) bool {
	if t.Root != nil {
		return t.Root.Kind == msg.BoardKindCmd
	}
	for _, r := range t.Replies {
		if r != nil && (r.Kind == msg.BoardKindCmd || r.Kind == msg.BoardKindAck) {
			return true
		}
	}
	return false
}

// Keeps reports whether v shows thread t. The zero View shows everything, so a
// caller that never chose a view loses nothing.
func (v View) Keeps(t Thread) bool {
	switch v {
	case ViewStatus:
		return !IsPromptThread(t)
	case ViewPrompt:
		return IsPromptThread(t)
	}
	return true
}

// FilterThreads keeps the threads v shows, in order.
func FilterThreads(ts []Thread, v View) []Thread {
	if v == "" || v == ViewAll || v == ViewFlow || v == ViewTeam {
		return ts
	}
	out := make([]Thread, 0, len(ts))
	for _, t := range ts {
		if v.Keeps(t) {
			out = append(out, t)
		}
	}
	return out
}

// CountViews counts a board's threads per view: (status, prompt).
func CountViews(ts []Thread) (status, prompt int) {
	for _, t := range ts {
		if IsPromptThread(t) {
			prompt++
		} else {
			status++
		}
	}
	return status, prompt
}

// FilterReply returns a copy of r showing only v's threads. Stats, roster and
// counters still describe the whole board — a view narrows what is listed,
// never what is claimed about the board.
func FilterReply(r *QueryReply, v View) *QueryReply {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Threads = FilterThreads(r.Threads, v)
	return &cp
}
