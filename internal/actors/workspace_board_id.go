// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Which board does a pane belong to? (design 028 §6.1, gate D-12.)
//
// THE ANSWER IS ON THE PANE, NEVER IN THE POST. A pane carries `board.id` meta;
// a pane without it belongs to the session board. That is what keeps founder
// gate 3 true — every claude may post, and a non-fleet claude needs no board
// meta to do it — while letting a fleet's panes share a board of their own.
//
// Meta is the right home for two reasons beyond the gate. It PERSISTS
// (PaneSnapshot.Meta rides the pane's KV snapshot and is restored on a daemon
// restart), so a board pane still knows which board it renders after a restart;
// and it is READABLE BY ANY TOOL that can ask for a snapshot, which is how
// fleetctl already reads fleet.role. A second copy in workspace state would be
// the thing that drifts.

// paneMetaBoardID is the pane meta key that names a pane's board.
const paneMetaBoardID = "board.id"

// boardForPane resolves the board a pane posts to and reads.
//
// Order: the pane's own `board.id` meta, then the session board. A fleet name
// is NOT consulted here — `fleet.name` becomes a board id only once the fleet
// actor owns that mapping (design 028 §6.5, `E-40`). Guessing it now would make
// the board a pane posts to depend on a convention no component enforces.
//
// An INVALID stored id resolves to the session board rather than to a subject
// nobody subscribes to. It cannot normally get there — every writing edge
// validates — but meta is free-form and writable by hand, so this reads it
// defensively.
func (w *WorkspaceActor) boardForPane(paneID string) string {
	if paneID == "" {
		return msg.DefaultBoardID
	}
	meta := w.paneMeta(paneID)
	if meta == nil {
		return msg.DefaultBoardID
	}
	id := strings.TrimSpace(meta[paneMetaBoardID])
	if id == "" || msg.ValidateBoardID(id) != nil {
		return msg.DefaultBoardID
	}
	return msg.NormalizeBoardID(id)
}

// resolveBoardArg turns a caller-supplied board name into a board id, or
// refuses. An empty argument means "the caller did not name one", which is not
// an error — the caller then falls back to boardForPane.
//
// `--fleet <name>` and `--board <id>` both land here: until the fleet actor
// exists, a fleet's board IS its name (design 028 §6.5, "BoardID == Name unless
// overridden"), so the two spellings resolve identically. The distinction is
// kept at the surface because it will stop being cosmetic in `E-40`.
func resolveBoardArg(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", nil
	}
	if err := msg.ValidateBoardID(arg); err != nil {
		return "", err
	}
	return msg.NormalizeBoardID(arg), nil
}

// extractBoardFlag pulls `--board <id>` / `--board=<id>` and the `--fleet`
// spelling out of a ## argument list.
//
// It returns the REMAINING words so the caller's own parsing is unchanged, and
// it reports a malformed flag rather than ignoring it: `##board post --board`
// with no value must not silently post to the session board, which is exactly
// the "flag that works in one position only" defect F-19 was filed for.
func extractBoardFlag(args []string) (rest []string, board string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--board" || a == "--fleet":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s needs a value", a)
			}
			board = args[i+1]
			i++
		case strings.HasPrefix(a, "--board="):
			board = strings.TrimPrefix(a, "--board=")
		case strings.HasPrefix(a, "--fleet="):
			board = strings.TrimPrefix(a, "--fleet=")
		default:
			rest = append(rest, a)
			continue
		}
	}
	if board != "" {
		id, verr := resolveBoardArg(board)
		if verr != nil {
			return nil, "", verr
		}
		return rest, id, nil
	}
	return rest, "", nil
}

// boardLabel names a board for a human reading a confirmation line. The session
// board is called by name rather than left blank, because "posted to the board"
// stops being unambiguous the moment a session has more than one.
func boardLabel(id string) string {
	if msg.IsDefaultBoard(id) {
		return msg.DefaultBoardID
	}
	return msg.NormalizeBoardID(id)
}
