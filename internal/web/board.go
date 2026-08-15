// SPDX-License-Identifier: Apache-2.0

// The agents board's READ path for clients that are not the terminal UI
// (design 025 §6, design 028, design 027 wave 1).
//
// WHY THIS FILE HAD TO EXIST. agents-board is a PaneType, and a pane of that
// type never starts a shell (domain.IsShelllessPaneType) — so nothing ever
// writes into its output buffer and there is nothing on the wire for a renderer
// to draw. The TUI does not notice, because it BUILDS the board itself:
// buildPanePanel dispatches the pane type to buildBoardPanel, which reads a
// board.Store the TUI subscribes to directly. Every other client got the pane's
// buffer, which for a shell-less pane holds whatever happened to be there —
// observed live in the desktop app as a board pane showing a stale
// `##pane list --meta` dump from a command that had errored against it, while
// four agents were posting to that board and their posts arrived fine.
//
// That is the worst shape a monitoring view can fail in: not blank, not an
// error, but PLAUSIBLE CONTENT that has nothing to do with the board. The
// operator's report was "I cannot see that the agent board is working" while
// the board was in fact working end to end.
//
// THE READER ASKS THE RECORDER. This does not subscribe, and it does not open
// the board's KV bucket — it puts a board.Query to ABLA over NATS, exactly as
// `rysh board tail` does. See msg.BoardQuerySubject for the two reasons
// (design 026 §5.4, and F-23, where a second call site derived the bucket name
// itself and read an empty board that looked healthy). A web server that grew
// its own subscription would be a third copy of the board in a process that is
// not the recorder, and would go stale exactly when the daemon is busiest.
//
// PULL, NOT PUSH, and that is a deliberate limit rather than an oversight. The
// board has no per-board change notification the server could forward, and
// adding a subscription here to synthesise one would be the third copy above.
// The client polls (AgentsBoardView) and the cost is bounded by the client
// count, not by the post rate.
package web

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// boardQueryTimeout bounds the round trip to the recorder.
//
// Generous on purpose. This runs off the read pump in its own goroutine, so a
// slow answer costs one goroutine and one late frame — while a timeout that
// fired early would report a WORKING board as unreachable, which is the same
// lie as an empty board with the sign flipped. `rysh board tail` uses the same
// order of magnitude for the same round trip.
const boardQueryTimeout = 3 * time.Second

// boardDefaultLimit bounds how many THREADS an unbounded request brings back.
//
// Threads rather than posts, because that is the unit board.Query bounds and
// the unit the store evicts by; asking for a post count would split threads
// from their replies. A client that wants more says so.
const boardDefaultLimit = 200

// handleBoardGet serves the ws "board_get" command: read one board and reply —
// to the REQUESTING client only — with a "board_result" carrying the request_id
// for correlation.
//
// Targeted like completion_get, and for a stronger reason than that one has:
// two windows can be showing DIFFERENT boards (design 028 gives every fleet its
// own), so a broadcast would deliver one fleet's board to a client rendering
// another's, keyed by a pane id it also holds. Each would then overwrite the
// pane it was asked about with somebody else's threads.
//
// AN UNANSWERED QUERY IS NOT AN EMPTY BOARD. board.ErrNoRecorder comes back to
// the client as `error`, never as `{"threads":[]}`, because an empty board and
// a dead recorder render identically and only one of them is a reason to go
// looking for the daemon. This is the rule board.Ask is built around
// (internal/board/query.go), and it survives this hop only if it is restated at
// the hop — a handler that dropped the error here would put the F-20/F-23
// failure back, one process further out.
func (s *Server) handleBoardGet(c *wsClient, data json.RawMessage) {
	var cmd struct {
		RequestID string `json:"request_id"`
		PaneID    string `json:"pane_id"`
		// Board is the pane's `board.id` meta, forwarded verbatim by the
		// client. It is resolved HERE through msg.BoardIDFromMeta rather than
		// trusted, so a renderer that sends nothing, or a hand-edited meta
		// value that could never be a subject token, lands on the session board
		// — the same answer the TUI shows for that pane, from the same
		// predicate.
		Board string `json:"board"`
		Since int64  `json:"since,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if json.Unmarshal(data, &cmd) != nil || cmd.RequestID == "" {
		return
	}
	if cmd.Limit <= 0 {
		cmd.Limit = boardDefaultLimit
	}
	boardID := msg.BoardIDFromMeta(cmd.Board)

	// s.nc is assigned once at construction and read unlocked by every other
	// subscriber in this package; it needs no lock and taking one here would
	// only suggest it does.
	nc := s.nc

	go func() {
		reply, err := board.Ask(nc, boardID, board.Query{
			Since: cmd.Since,
			Limit: cmd.Limit,
		}, boardQueryTimeout)

		// TWO MARSHAL CALL SITES WITH INLINE LITERALS, not one map assembled
		// through a variable. The ws-protocol golden (ws_protocol_test.go)
		// reads these literals STATICALLY to enumerate what the server speaks;
		// a `data` built up field by field and passed in by name degrades to an
		// opaque `data_go_expr`, and every field in this reply silently stops
		// being covered by the spec the TypeScript clients are written against.
		// The duplicated correlation keys are the price of that coverage.
		if err != nil {
			// The whole point of the error branch: say WHY there is nothing to
			// show. no_recorder is separated from every other failure because
			// it is the one with an operator action attached — the recorder
			// (ABLA) is not answering for this session — while a refused or
			// unreadable query is a bug in this hop. It is its own boolean
			// rather than something the client pattern-matches out of the
			// message text, which would break the first time the wording
			// improves. board.Ask wraps with %w, so this is errors.Is.
			//
			// NOTE WHAT IS NOT HERE: threads. Not an empty list — absent.
			frame, mErr := json.Marshal(map[string]interface{}{
				"type": "board_result",
				"data": map[string]interface{}{
					"request_id":  cmd.RequestID,
					"pane_id":     cmd.PaneID,
					"board":       boardID,
					"error":       err.Error(),
					"no_recorder": errors.Is(err, board.ErrNoRecorder),
				},
			})
			if mErr == nil {
				sendToClient(c, frame)
			}
			return
		}

		// Threads/Roster are never nil from board.Answer, but a reply decoded
		// off the wire can carry JSON null. Normalising here keeps `.map` on
		// the client from throwing on a board nobody has posted to yet.
		threads := reply.Threads
		if threads == nil {
			threads = []board.Thread{}
		}
		roster := reply.Roster
		if roster == nil {
			roster = []board.RosterEntry{}
		}
		frame, mErr := json.Marshal(map[string]interface{}{
			"type": "board_result",
			"data": map[string]interface{}{
				"request_id":        cmd.RequestID,
				"pane_id":           cmd.PaneID,
				"board":             boardID,
				"threads":           threads,
				"roster":            roster,
				"stats":             reply.Stats,
				"filtered":          reply.Filtered,
				"withheld":          reply.Withheld,
				"roster_reconciled": reply.RosterReconciled,
			},
		})
		if mErr == nil {
			sendToClient(c, frame)
		}
	}()
}

// sendToClient hands one frame to one connection, dropping it if that
// connection is gone or too far behind.
//
// Dropping is right for this message specifically: the client POLLS, so a lost
// frame costs one refresh interval and nothing else. A blocking send here would
// park a goroutine on a socket nobody is reading.
func sendToClient(c *wsClient, frame []byte) {
	if c == nil {
		return
	}
	select {
	case c.send <- frame:
	default:
	}
}
