// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

// peerPublishCommand is `peer publish` / `peer unpublish` (design 044
// amendment 2026-10-20, N3): the explicit, per-board opt-in that lets a local
// board's posts reach the peer board. The farm's publisher reads the saved
// list before every send, so a change here takes effect at once. A managed
// peer task cannot reach this: its capability allows status/tail/post/reply
// only (peercoord commandControl).
func (w *WorkspaceActor) peerPublishCommand(p peer.Args, c peer.Connection) *msg.MsgCLIResponse {
	fail := func(err error) *msg.MsgCLIResponse { return &msg.MsgCLIResponse{Error: err.Error()} }
	pubs, err := peer.LoadPublications(w.cfg.RyshDir, w.cfg.SessionName)
	if err != nil {
		return fail(fmt.Errorf("load peer publications: %w", err))
	}
	switch {
	case p.Verb == "publish" && p.LocalBoard == "":
		if p.JSON {
			data, err := json.Marshal(pubs)
			if err != nil {
				return fail(err)
			}
			return &msg.MsgCLIResponse{OK: true, Output: string(data) + "\n"}
		}
		if len(pubs.Boards) == 0 {
			return &msg.MsgCLIResponse{OK: true, Output: "no local board is published; peer publish <board> opts one in\n"}
		}
		var out strings.Builder
		fmt.Fprintf(&out, "published to peer board %s:\n", c.Board)
		for _, name := range pubs.Names() {
			pub := pubs.Boards[name]
			kinds := ""
			if pub.All {
				kinds = " (all kinds)"
			}
			fmt.Fprintf(&out, "  %s%s since %s\n", name, kinds, time.UnixMilli(pub.Since).Local().Format("2006-01-02 15:04:05"))
		}
		return &msg.MsgCLIResponse{OK: true, Output: out.String()}
	case p.Verb == "publish":
		if !c.Enabled {
			return fail(fmt.Errorf("peer board disconnected; use ##peer connect before publishing a board"))
		}
		pubs.Boards[p.LocalBoard] = peer.Publication{Board: p.LocalBoard, Since: time.Now().UnixMilli(), All: p.All}
		if err := peer.SavePublications(w.cfg.RyshDir, w.cfg.SessionName, pubs); err != nil {
			return fail(err)
		}
		kinds := "status posts"
		if p.All {
			kinds = "posts of every kind"
		}
		return &msg.MsgCLIResponse{OK: true, Output: fmt.Sprintf("local board %s is now published to peer board %s: its %s from now on are sent as text (no backfill, no terminal output); peer unpublish %s stops it\n", p.LocalBoard, c.Board, kinds, p.LocalBoard)}
	default: // unpublish
		if _, ok := pubs.Boards[p.LocalBoard]; !ok {
			return fail(fmt.Errorf("local board %s is not published", p.LocalBoard))
		}
		delete(pubs.Boards, p.LocalBoard)
		if err := peer.SavePublications(w.cfg.RyshDir, w.cfg.SessionName, pubs); err != nil {
			return fail(err)
		}
		return &msg.MsgCLIResponse{OK: true, Output: fmt.Sprintf("local board %s is no longer published; nothing more is sent\n", p.LocalBoard)}
	}
}
