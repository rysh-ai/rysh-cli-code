// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##board copy` / `##board output` — messages out of the board (the
// board-copy task). Item-based: one item = one agent message.
//
// The daemon does the whole command except the last hop. It asks the recorder
// (board.Ask — the read path's one rule: ask ABLA, never read the KV), selects
// and formats through internal/board's shared copy pipeline (the same one the
// board pane's y/Y and `rysh board tail --format` use), and then HANDS THE
// TEXT to an attached TUI over msg.ClipboardWriteSubject, because only a TUI
// owns the tty OSC 52 has to travel down. No TUI answering — a web-only
// session, a `rysh send` — falls back to this host's clipboard, and the
// confirmation says which path the copy took.
//
// REFUSALS ARE LOUD. A recorder that did not answer is reported with its error
// and nothing is copied; so is a selection that came out empty. Neither ever
// becomes an empty clipboard written as though the board were empty — the
// discipline `rysh board tail` follows (cmd/rysh/board_cmd.go).

// boardCopyQueryTimeout bounds the recorder round trip; same order as the web
// board view's, which answers the same question.
const boardCopyQueryTimeout = 3 * time.Second

// clipboardHandoffTimeout bounds the wait for a TUI to take the copy. A TUI
// answers from its subscription goroutine, so this is a bus round trip, not a
// frame; missing it only means the local fallback is used.
const clipboardHandoffTimeout = 1500 * time.Millisecond

// askBoard and writeLocalClipboard are seams for tests.
var (
	askBoard            = board.Ask
	writeLocalClipboard = clipboard.WriteAll
)

// handleBoardCopy runs `##board copy [worker] [N]` for the pane it was typed
// in. boardID is already resolved (--board on the line, else the pane's board).
func (w *WorkspaceActor) handleBoardCopy(out *strings.Builder, paneID, boardID string, args []string) error {
	a, err := board.ParseItemArgs(args, false)
	if err != nil {
		return w.boardItemUsage(out, "##board copy: "+err.Error())
	}
	if a.Board != "" {
		boardID = a.Board
	}
	reply, err := askBoard(w.nc, boardID, board.Query{}, boardCopyQueryTimeout)
	if err != nil {
		return w.boardCopyRefused(out, boardCopyAskError(err))
	}
	res, err := board.BuildCopy(reply, a.Worker, a.N)
	if err != nil {
		return w.boardItemRefused(out, err)
	}
	via, err := w.deliverClipboard(paneID, res.Text)
	if err != nil {
		return w.boardCopyRefused(out, err.Error())
	}
	fmt.Fprintf(out, "%s (%s)%s\n", res.Summary(), via, windowNote(reply))
	return nil
}

// handleBoardOutput runs `##board output [worker] [N] html|pdf`: the same
// items as copy, rendered by the shared HTML renderer to a file on this host,
// printed to PDF if asked, and opened.
func (w *WorkspaceActor) handleBoardOutput(out *strings.Builder, boardID string, args []string) error {
	a, err := board.ParseItemArgs(args, true)
	if err != nil {
		if errors.Is(err, board.ErrNeedsFormat) {
			fmt.Fprintf(out, "[board] %v\n", err)
			w.failRyshUsage("%v", err)
			return err
		}
		return w.boardItemUsage(out, "##board output: "+err.Error())
	}
	if a.Board != "" {
		boardID = a.Board
	}
	reply, err := askBoard(w.nc, boardID, board.Query{}, boardCopyQueryTimeout)
	if err != nil {
		return w.boardCopyRefused(out, boardCopyAskError(err))
	}
	items := board.Flatten(reply)
	picked, matched, total := board.Select(items, a.Worker, a.N)
	if len(picked) == 0 {
		return w.boardItemRefused(out, board.ErrNoMessages{Worker: a.Worker})
	}
	path, err := writeBoardOutput(board.OutputRequest{
		Page: board.HTMLPage{Board: boardLabel(boardID), Worker: a.Worker, N: a.N,
			Picked: picked, Matched: matched, Total: total, Now: time.Now()},
		Format: a.Format,
		Dir:    a.Out,
		Open:   !a.NoOpen,
	})
	if err != nil {
		fmt.Fprintf(out, "%v\n", err)
		w.failRysh("%v", err)
		return err
	}
	fmt.Fprintf(out, "%s%s\n", board.OutputSummary(len(picked), matched, a.Worker, path), windowNote(reply))
	return nil
}

// writeBoardOutput is a seam for tests.
var writeBoardOutput = board.WriteOutput

// windowNote says when the recorder's window was not the whole board.
func windowNote(r *board.QueryReply) string {
	if notes := board.TruncationNotes(r); notes != "" {
		return " — window truncated: " + notes + "; this is NOT the whole board"
	}
	return ""
}

// boardItemRefused prints an empty selection or an oversized copy as the
// line the shared model words it as.
func (w *WorkspaceActor) boardItemRefused(out *strings.Builder, err error) error {
	line := err.Error()
	if !strings.HasPrefix(line, "[board]") {
		line = "[board] " + line
	}
	fmt.Fprintf(out, "%s\n", line)
	w.failRysh("%s", line)
	return err
}

// boardItemUsage prints the copy AND output grammar: the task's rule is that
// either command with bad arguments shows both.
func (w *WorkspaceActor) boardItemUsage(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "%s\n", why)
	writeBoardItemUsage(out)
	w.failRyshUsage("%s", why)
	return fmt.Errorf("%s", why)
}

func writeBoardItemUsage(out *strings.Builder) {
	out.WriteString("  ##board " + board.CopyUsage + "\n")
	out.WriteString("  ##board " + board.OutputUsage + "\n")
	for _, l := range board.UsageLines {
		out.WriteString("      ##board " + l + "\n")
	}
}

// boardCopyAskError words an unanswered query the way the read path always
// does: it is not an empty board.
func boardCopyAskError(err error) string {
	if errors.Is(err, board.ErrNoRecorder) {
		return "the board recorder did not answer — THIS IS NOT AN EMPTY BOARD, nothing can be said about what was posted (" +
			err.Error() + ")"
	}
	return err.Error()
}

func (w *WorkspaceActor) boardCopyRefused(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "[board] copy refused: %s\n", why)
	w.failRysh("board copy refused: %s", why)
	return fmt.Errorf("board copy refused: %s", why)
}

// deliverClipboard hands text to an attached TUI for OSC 52, falling back to
// this host's clipboard. It returns which path took the copy.
func (w *WorkspaceActor) deliverClipboard(paneID, text string) (string, error) {
	var tuiErr string
	if w.pub != nil {
		reply, err := w.pub.Request(msg.ClipboardWriteSubject(),
			&msg.MsgClipboardWrite{PaneID: paneID, Text: text}, clipboardHandoffTimeout)
		if err == nil {
			if resp, ok := reply.(*msg.MsgCLIResponse); ok {
				if resp.OK {
					via := strings.TrimSpace(resp.Output)
					if via == "" {
						via = "terminal clipboard (OSC 52)"
					}
					return via, nil
				}
				tuiErr = resp.Error
			}
		}
	}
	if err := writeLocalClipboard(text); err != nil {
		if tuiErr != "" {
			return "", fmt.Errorf("the terminal could not copy (%s) and the local clipboard failed: %v", tuiErr, err)
		}
		return "", fmt.Errorf("no terminal is attached to take the copy, and the local clipboard failed: %v", err)
	}
	return "local clipboard (no terminal attached for OSC 52)", nil
}
