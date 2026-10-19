// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A board pane's clipboard_copy returns the BOARD, unwrapped, through the same
// formatter as `##board copy` — not the shell-less pane's stale buffer.

func newBoardClipboardServer(t *testing.T, st *board.Store, snap *domain.PaneSnapshot) *Server {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23235, "board-clip-session", pub, nc, pub.Codecs())
	s.hub = newHub()
	rec := &paneRequestRecorder{t: t, nc: nc, seen: make(chan string, 4), fullSnap: snap}
	rec.serve(msg.T("pane", snap.ID, "inbox"))
	if st != nil {
		subs, err := board.ServeQueries(nc, func(string) *board.Store { return st }, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, sub := range subs {
				_ = sub.Unsubscribe()
			}
		})
	}
	return s
}

func TestClipboardCopyOfABoardPaneIsTheSharedBoardText(t *testing.T) {
	st := board.New(0)
	p := msg.NewBoardPost("p314", "issue-314", msg.BoardKindMilestone,
		"RUN: https://ci.example.com/"+strings.Repeat("r", 190)+"\nSTATUS: green", 1_000)
	p.ThreadID = msg.MintThreadID("p314", 1)
	st.Apply(p)

	snap := &domain.PaneSnapshot{ID: "bp", PaneType: domain.PaneTypeAgentsBoard,
		Output: "stale ##pane list dump"}
	s := newBoardClipboardServer(t, st, snap)

	reply := board.Answer(st, board.Query{})
	res, err := board.BuildCopy(&reply, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := res.Text
	for _, params := range []string{`{"request_id":"r1","pane_id":"bp"}`, `{"request_id":"r2","pane_id":"bp","source":"board"}`} {
		_, data := copyRequest(t, s, params)
		if got := data["text"].(string); got != want {
			t.Fatalf("%s: text = %q, want the shared board text %q", params, got, want)
		}
		if data["err"].(string) != "" || data["truncated"].(bool) {
			t.Fatalf("%s: err %q truncated %v", params, data["err"], data["truncated"])
		}
	}
}

func TestClipboardCopyOfABoardPaneWithoutARecorderIsAnError(t *testing.T) {
	snap := &domain.PaneSnapshot{ID: "bp", PaneType: domain.PaneTypeAgentsBoard, Output: "stale"}
	s := newBoardClipboardServer(t, nil, snap)
	_, data := copyRequest(t, s, `{"request_id":"r1","pane_id":"bp"}`)
	if data["err"].(string) == "" || data["text"].(string) != "" {
		t.Fatalf("no recorder answered with text %q err %q; want an error and no text", data["text"], data["err"])
	}
}

func TestClipboardBoardSourceNeedsABoardPane(t *testing.T) {
	s := newBoardClipboardServer(t, board.New(0), &domain.PaneSnapshot{ID: "p1", Output: "hi"})
	_, data := copyRequest(t, s, `{"request_id":"r1","pane_id":"p1","source":"board"}`)
	if data["err"].(string) == "" {
		t.Fatal("source board on a shell pane must be an error")
	}
}

// TestClipboardCeilingMatchesTheBoardCopyCap pins the two limits together.
func TestClipboardCeilingMatchesTheBoardCopyCap(t *testing.T) {
	if clipboardMaxBytes != board.CopyMaxBytes {
		t.Fatalf("clipboardMaxBytes = %d, board.CopyMaxBytes = %d; one surface would refuse what another copies",
			clipboardMaxBytes, board.CopyMaxBytes)
	}
}
