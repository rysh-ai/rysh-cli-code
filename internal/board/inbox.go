// SPDX-License-Identifier: Apache-2.0

package board

// The command inbox (design 050, polling-stream): which commands on a board are
// addressed to one pane, and which of them it has answered.
//
// This is a READ for an agent that wants context — "what was I asked earlier?"
// — and never a loop. The courier delivers every command into the agent's own
// session; the founder ruled (2026-10-11) that agents may read the board on
// demand but never watch it, so nothing here follows, waits or polls.
//
// Pure over a QueryReply, like Answer: no NATS, no clock.

import (
	"sort"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// InboxItem is one command addressed to the reader.
type InboxItem struct {
	CmdID      string `json:"cmd_id"`
	ThreadID   string `json:"thread_id"`
	From       string `json:"from"`         // sender persona, display only
	FromPaneID string `json:"from_pane_id"` // sender identity
	TS         int64  `json:"ts"`
	Text       string `json:"text"`
	File       string `json:"file,omitempty"`

	// Answered is true once the reader acked the command or posted anything
	// on its thread. A reply is an answer: design 050 §5.4 makes
	// `fleetctl report` the ack, and a report is a post on that thread.
	Answered bool  `json:"answered"`
	AckTS    int64 `json:"ack_ts,omitempty"` // the first answer's time
}

// Inbox lists the commands on a board addressed to paneID (a FULL pane uuid),
// newest first. A command counts once even if a retried send put it on the
// board twice: the identity is (sender, CmdID).
func Inbox(r *QueryReply, paneID string) []InboxItem {
	if r == nil || paneID == "" {
		return nil
	}

	var posts []*msg.MsgBoardPost
	for _, t := range r.Threads {
		if t.Root != nil {
			posts = append(posts, t.Root)
		}
		posts = append(posts, t.Replies...)
	}

	type key struct{ from, id string }
	items := map[key]*InboxItem{}
	for _, p := range posts {
		if p == nil || p.Kind != msg.BoardKindCmd || p.ToPaneID != paneID || p.CmdID == "" {
			continue
		}
		k := key{p.PaneID, p.CmdID}
		if _, seen := items[k]; seen {
			continue
		}
		items[k] = &InboxItem{
			CmdID: p.CmdID, ThreadID: p.ThreadID,
			From: p.Persona, FromPaneID: p.PaneID,
			TS: p.TS, Text: p.Text, File: p.File,
		}
	}

	// An answer is any post BY the reader on a command's thread, other than
	// the command itself (a reader cannot be a command's sender: the door
	// refuses self-addressed commands). An ack also names the command, which
	// makes it count even when the command was filed under a shared thread.
	for _, p := range posts {
		if p == nil || p.PaneID != paneID || p.Kind == msg.BoardKindCmd {
			continue
		}
		for _, it := range items {
			hit := p.ThreadID != "" && p.ThreadID == it.ThreadID
			if p.Kind == msg.BoardKindAck {
				hit = p.CmdID == it.CmdID && (p.ThreadID == it.ThreadID || p.ThreadID == "")
			}
			if hit && (!it.Answered || p.TS < it.AckTS) {
				it.Answered, it.AckTS = true, p.TS
			}
		}
	}

	out := make([]InboxItem, 0, len(items))
	for _, it := range items {
		out = append(out, *it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS > out[j].TS
		}
		return out[i].CmdID > out[j].CmdID
	})
	return out
}

// FindCommand returns the command cmdID addressed to paneID, if the board holds
// it. When two senders used the same id, the newest wins — `board ack` names
// only an id, and the newest is the one the reader was just handed.
func FindCommand(r *QueryReply, paneID, cmdID string) (InboxItem, bool) {
	for _, it := range Inbox(r, paneID) {
		if it.CmdID == cmdID {
			return it, true
		}
	}
	return InboxItem{}, false
}
