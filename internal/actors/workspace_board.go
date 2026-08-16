// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// agents-board (design 025 §4) — the two doors a post arrives through, and why
// there are two.
//
//   - handleCLIBoardPost is the AGENT door (msg.MsgCLIBoardPost, reached from
//     the workspace's CLI switch). It is SILENT: it focuses nothing, writes into
//     no pane's buffers, and never infers its poster. Dozens of agents post
//     milestones concurrently, and any one of those three behaviours would make
//     the session unusable for the human watching the board.
//
//   - handleBoardCommand is the HUMAN door (`##board`, typed into a pane). It
//     runs through the ordinary ## machinery, so it DOES echo into the caller's
//     pane and IS attributed to the pane the human typed in. That is wanted
//     here: a human who types a command expects to see it and its reply.
//
// The agent door exists because the ## CLI path has three properties a board
// post must not inherit. All three were verified in wave 1 and re-verified
// against this tree before this file was written:
//
//  1. AMBIENT ATTRIBUTION. handleCLIRyshCommand with neither PaneID nor TabID
//     runs as the workspace's ACTIVE pane — the `default:` branch taking
//     w.activePaneID in workspace_rysh.go. A post would be credited to a
//     bystander.
//  2. FOCUS THEFT. Naming a pane calls w.focusPaneByID, which switches the
//     active tab and moves the human's cursor (workspace_rysh.go, both the
//     PaneID and the TabID branches).
//  3. OUTPUT ECHO. runRyshCommand writes the command line into the pane's rysh
//     history, its mode history, SendPaneOutput and SendPaneRyshOutput before
//     it dispatches anything (workspace_rysh.go). A post would land in that
//     pane's scrollback — under hazard 1, a bystander's.
//
// Hazards 1 and 3 compound: a post would pollute a pane that had nothing to do
// with it. So handleCLIBoardPost never routes through runRyshCommand, and the
// two headline tests in workspace_board_test.go pin exactly that — one on
// focus, one on every pane subject the publisher can reach.

// A board post carries persona and thread, and nothing about the fleet: no
// envelope, no fleet name, no role, no unit (founder gate 4). A post is chat —
// who spoke, under which thread — and `FROM manager … TO worker …` is fleet
// routing, which is a different concern.
//
// That absence is load-bearing rather than incidental. Founder gate 3 says
// EVERY claude in the session may post, not only fleet members; with no fleet
// field in the schema at all there is no field that can be missing for a
// non-fleet poster, so nothing downstream can quietly learn to treat fleet
// posts as first-class. Neither door reads fleet.* pane meta, and neither may
// smuggle the envelope through the post text.

// handleCLIBoardPost is the agent-facing board post. It resolves the poster,
// builds the message and publishes it — and does nothing else, deliberately.
//
// It takes no actor.Context because it changes no workspace state: there is
// nothing to persist and nothing to focus. That is not an oversight, it is the
// property under test.
func (w *WorkspaceActor) handleCLIBoardPost(m *msg.MsgCLIBoardPost) *msg.MsgCLIResponse {
	if m == nil {
		return &msg.MsgCLIResponse{OK: false, Error: "empty board post"}
	}

	// AsPaneID is required and has NO fallback to the active pane. Silently
	// attributing a post to whichever pane happened to be focused is the exact
	// defect MsgCLIBoardPost exists to prevent (hazard 1), so an empty value is
	// an error even though the ambient pane is sitting right there in
	// w.activePaneID.
	as := strings.TrimSpace(m.AsPaneID)
	if as == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "--as <pane-id> is required: a board post " +
			"names its poster explicitly and never falls back to the active pane"}
	}

	text := strings.TrimSpace(m.Text)
	if text == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "empty board post text"}
	}

	// resolvePaneID accepts an id, an auto-title or a given-name and — unlike
	// handleCLIRyshCommand's resolution — does not focus what it finds.
	paneID := w.resolvePaneID(as)
	if paneID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("pane not found: %s", as)}
	}

	// WHICH BOARD (design 028): the request's own --board if it named one,
	// otherwise the poster pane's `board.id` meta, otherwise the session board.
	// A named board that does not validate is REFUSED rather than downgraded to
	// the session board — an agent told its milestone landed on its fleet's
	// board when it landed somewhere else is the receipt-without-delivery
	// failure this whole stack keeps re-learning.
	boardID := w.boardForPane(paneID)
	if named := strings.TrimSpace(m.Board); named != "" {
		id, err := resolveBoardArg(named)
		if err != nil {
			return &msg.MsgCLIResponse{OK: false, Error: err.Error()}
		}
		boardID = id
	}

	post := w.buildBoardPost(paneID, m.Kind, text, m.ThreadID)
	if err := msg.SendBoardPost(w.pub, boardID, post); err != nil {
		return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("publish board post: %v", err)}
	}
	return &msg.MsgCLIResponse{OK: true, ID: post.ThreadID, Output: boardPostConfirmation(boardID, post)}
}

// buildBoardPost assembles one post from a pane's live snapshot. Shared by both
// doors so the human-typed path and the agent path can never disagree about how
// a persona is resolved or where fleet context comes from.
func (w *WorkspaceActor) buildBoardPost(paneID, kind, text, threadID string) *msg.MsgBoardPost {
	var givenName, title string
	if ps := w.paneSnapshotByID(paneID); ps != nil {
		givenName, title = ps.GivenName, ps.Title
	}

	// Always through BoardPersona, never the raw given-name: approval panes
	// overload GivenName to carry "requestID\x1FresponseSubject", and rendering
	// that blindly would one day print a NATS subject as somebody's name
	// (design 025 §5). BoardPersona applies the fallback chain and the \x1f
	// guard in one place.
	post := msg.NewBoardPost(
		paneID,
		msg.BoardPersona(givenName, title, paneID),
		kind,
		text,
		time.Now().UnixMilli(),
	)
	post.ThreadID = strings.TrimSpace(threadID)
	return post
}

// boardPostConfirmation is prose on purpose. Design 025 §4.3 makes thread ids
// AGENT-minted precisely so no client has to parse a round trip; this line is a
// confirmation nobody is required to read, and an agent that does want the id in
// a field reads MsgCLIResponse.ID instead of scraping this.
//
// It names the board since 028: "posted to the board" was unambiguous while
// there was one, and is a guess as soon as a session has several.
func boardPostConfirmation(boardID string, post *msg.MsgBoardPost) string {
	if post.ThreadID == "" {
		return fmt.Sprintf("posted to board %s as %s (%s): root, no thread id\n",
			boardLabel(boardID), post.Persona, post.Kind)
	}
	return fmt.Sprintf("posted to board %s as %s (%s): thread %s\n",
		boardLabel(boardID), post.Persona, post.Kind, post.ThreadID)
}

// handleBoardCommand is the `##board` verb — the HUMAN front door.
//
// Unlike handleCLIBoardPost this runs inside the normal ## dispatch, so the
// command line is echoed into the calling pane and the result is written back
// to it. That echo is the difference between the two doors and it is wanted
// here: the poster is the human's own pane, so there is no bystander to
// pollute and no focus to steal — the pane is already focused, because someone
// just typed into it.
func (w *WorkspaceActor) handleBoardCommand(out *strings.Builder, paneID string, args []string) error {
	// `--board <id>` / `--fleet <name>` may appear anywhere in any subcommand,
	// and an unparseable one stops the command rather than falling back to the
	// session board (F-19's rule: a flag that silently does nothing is worse
	// than one that refuses).
	rest, flagBoard, berr := extractBoardFlag(args)
	if berr != nil {
		return w.boardUsage(out, fmt.Sprintf("##board: %v", berr))
	}
	args = rest
	// Read the subcommand only after the flags are stripped: an earlier read
	// would be overwritten here, which is what ineffassign was pointing at.
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	// The board this command is about: the one named on the line, else the
	// caller's own board, else the session board.
	boardID := flagBoard
	if boardID == "" {
		boardID = w.boardForPane(paneID)
	}

	switch sub {
	case "open":
		// The human's door onto the board. Opening is a pane operation, not a
		// post, so it lives in workspace_board_pane.go and shares nothing with
		// the posting path -- an agent posting must never create panes.
		w.openAgentsBoardPane(out, paneID, boardID)
		return nil

	case "agent":
		// The board claude: a real pane kept off screen (design 027 §2 ruling 1).
		return w.handleBoardAgentCommand(out, paneID, boardID, args[1:])

	case "post":
		body := strings.TrimSpace(strings.Join(args[1:], " "))
		if body == "" {
			return w.boardUsage(out, "##board post needs some text")
		}
		return w.boardPostFromPane(out, paneID, boardID, "", body)

	case "reply":
		// `##board reply <thread> <text>` — the thread key is explicit. There is
		// deliberately no "reply to my last root" shorthand here: it would need
		// per-pane state the agent door does not have, and the two doors must
		// not diverge on what a thread id means.
		if len(args) < 3 {
			return w.boardUsage(out, "##board reply needs a thread id and some text")
		}
		body := strings.TrimSpace(strings.Join(args[2:], " "))
		if body == "" {
			return w.boardUsage(out, "##board reply needs some text")
		}
		return w.boardPostFromPane(out, paneID, boardID, args[1], body)

	default:
		if sub == "" {
			return w.boardUsage(out, "##board needs a subcommand")
		}
		return w.boardUsage(out, fmt.Sprintf("unknown ##board subcommand: %q", sub))
	}
}

// boardPostFromPane publishes on behalf of the pane the command was typed in,
// onto the board that pane belongs to unless the line named another.
func (w *WorkspaceActor) boardPostFromPane(out *strings.Builder, paneID, boardID, threadID, text string) error {
	if paneID == "" {
		w.failRysh("##board has no pane to post as")
		fmt.Fprintf(out, "##board: no pane to post as\n")
		return fmt.Errorf("##board has no pane to post as")
	}
	kind := msg.BoardKindMilestone
	if threadID != "" {
		kind = msg.BoardKindReply
	}
	post := w.buildBoardPost(paneID, kind, text, threadID)
	if err := msg.SendBoardPost(w.pub, boardID, post); err != nil {
		w.failRysh("publish board post: %v", err)
		fmt.Fprintf(out, "##board: publish failed: %v\n", err)
		return fmt.Errorf("publish board post: %w", err)
	}
	out.WriteString(boardPostConfirmation(boardID, post))
	return nil
}

// boardUsage reports a wrong invocation both ways: prose for the human reading
// the pane, and an error so a script's exit code means something. The verb's
// table entry claims statusAware, and this is what earns it.
func (w *WorkspaceActor) boardUsage(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "%s\n", why)
	out.WriteString("  ##board open [--board <id>]    open the agents board pane (default board: session)\n")
	out.WriteString("  ##board post <text>            post a milestone to the agents board\n")
	out.WriteString("  ##board reply <thread> <text>  reply under an existing thread\n")
	out.WriteString("  ##board agent up               start the board claude in a hidden pane\n")
	out.WriteString("  ##board agent visible          show the board claude's pane\n")
	out.WriteString("  ##board agent invisible        hide it again (it keeps running)\n")
	w.failRyshUsage("%s", why)
	return fmt.Errorf("%s", why)
}
