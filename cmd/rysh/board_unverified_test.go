// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// W3 (design 030 §9.3): `rysh board tail` marks a post the recorder tagged.
func TestBoardTailMarksUnverifiedPosts(t *testing.T) {
	reply := &board.QueryReply{Threads: []board.Thread{{
		Key:     "t/1",
		Root:    &msg.MsgBoardPost{PaneID: "p1", Persona: "worker-1", Kind: "milestone", Text: "claims", Unverified: true},
		Replies: []*msg.MsgBoardPost{{PaneID: "p2", Persona: "ceo", Kind: "reply", Text: "ok"}},
	}}}
	out := renderBoardTail(reply)
	if !strings.Contains(out, "worker-1 (unverified)") {
		t.Errorf("tail does not mark the unverified root:\n%s", out)
	}
	if strings.Contains(out, "ceo (unverified)") {
		t.Errorf("tail marks a verified reply:\n%s", out)
	}
}
