// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The board claude — `##board agent …` (design 027).
//
// The board claude is a NORMAL pane running an interactive claude, kept off
// screen by domain.PaneSnapshot.Hidden. A real pane deliberately: it can be
// watched, taken over and debugged like any other agent, and "hidden" is a
// visibility state rather than a headless process (design 027 §2, ruling 1).
//
// It is found by GIVEN-NAME, not by a remembered id, for the reason
// workspace.go gives for boardPaneID: an id kept in a field is a fact that can
// go stale, and the live snapshot cannot. Given-names are unique per LANE and
// not per session (tab_snapshot.go IsGivenNameTakenInLane), so two panes may
// legally be called `board` — in which case this REFUSES and prints the
// candidates. Refusing is always correct here; guessing is never (design 026
// §5.1). A command that silently picked one would hide the wrong agent.

// boardAgentName is the given-name the board claude answers to.
const boardAgentName = "board"

// boardAgentNameFor is the given-name the board claude of one board answers to:
// `board` for the session board, `board-<id>` for a fleet's (design 028,
// `D-13`).
//
// A NAME RATHER THAN A LOOKUP, because the pane's given-name is what survives a
// daemon restart and what a human sees in `##pane list`. The alternative —
// finding every pane called `board` and reading its meta — needs a snapshot per
// tab to answer a question the name already answers, and it makes two fleets'
// board claudes indistinguishable on screen.
func boardAgentNameFor(boardID string) string {
	if msg.IsDefaultBoard(boardID) {
		return boardAgentName
	}
	return boardAgentName + "-" + msg.NormalizeBoardID(boardID)
}

// boardAgentClaudeArgs are the flags the board claude is launched with. See
// boardAgentUp for why it is autonomous and what that costs.
const boardAgentClaudeArgs = "--dangerously-skip-permissions"

// boardAgentPanes returns every pane called `board`, across every tab.
func (w *WorkspaceActor) boardAgentPanes(boardID string) []domain.PaneSnapshot {
	want := boardAgentNameFor(boardID)
	var found []domain.PaneSnapshot
	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			// A tab that cannot answer is not an empty tab. Reporting it as one
			// would turn "I could not look" into "it is not there" — the
			// distinction workspace_ansa.go's Panes() is built around.
			continue
		}
		for p := range domain.PanesInTab(tabSnap) {
			if p.GivenName == want {
				found = append(found, *p)
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ID < found[j].ID })
	return found
}

// tabOfPane returns the id of the tab holding paneID, or "".
func (w *WorkspaceActor) tabOfPane(paneID string) string {
	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		if p := domain.FindPaneInTab(tabSnap, paneID); p != nil {
			return info.id
		}
	}
	return ""
}

// resolveBoardAgent finds the single board claude, or explains why it cannot.
func (w *WorkspaceActor) resolveBoardAgent(boardID string) (domain.PaneSnapshot, error) {
	return resolveBoardAgentNamed(w.boardAgentPanes(boardID), boardAgentNameFor(boardID))
}

// resolveBoardAgentFrom is the addressing rule, split out from the lookup so it
// can be tested without a live workspace. The rule is 026 §5.1 applied to one
// name: exactly one pane resolves, zero is an error, and two or more is a
// REFUSAL naming the candidates. Refusing is always correct; guessing never is.
func resolveBoardAgentFrom(panes []domain.PaneSnapshot) (domain.PaneSnapshot, error) {
	return resolveBoardAgentNamed(panes, boardAgentName)
}

// resolveBoardAgentNamed is the same rule for a NAMED board's claude, so the
// refusal can say which name it was looking for — with several fleets running,
// "no pane named board" would be true and useless.
func resolveBoardAgentNamed(panes []domain.PaneSnapshot, want string) (domain.PaneSnapshot, error) {
	switch len(panes) {
	case 0:
		return domain.PaneSnapshot{}, fmt.Errorf(
			"no pane named %q — that board's claude is not running", want)
	case 1:
		return panes[0], nil
	default:
		ids := make([]string, 0, len(panes))
		for _, p := range panes {
			ids = append(ids, shortID(p.ID))
		}
		return domain.PaneSnapshot{}, fmt.Errorf(
			"%d panes are named %q (%s) — refusing to guess which one is the board claude",
			len(panes), want, strings.Join(ids, ", "))
	}
}

// handleBoardAgentCommand implements `##board agent visible|invisible|status`.
func (w *WorkspaceActor) handleBoardAgentCommand(out *strings.Builder, paneID, boardID string, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	switch sub {
	case "up":
		return w.boardAgentUp(out, paneID, boardID)
	case "visible":
		return w.setBoardAgentHidden(out, boardID, false)
	case "invisible":
		return w.setBoardAgentHidden(out, boardID, true)
	case "status":
		return w.boardAgentStatus(out, boardID)
	case "":
		return w.boardAgentUsage(out, "##board agent needs a subcommand")
	default:
		return w.boardAgentUsage(out, fmt.Sprintf("unknown ##board agent subcommand: %q", sub))
	}
}

// setBoardAgentHidden takes the board claude off screen, or puts it back.
//
// REVEALING DOES NOT MOVE FOCUS, and that is a decision rather than an
// omission: `##board agent visible` means "draw it", not "take me to it". Every
// other pane-affecting `##` command in this file already works that way
// (openAgentsBoardPane restores focus after creating), and the alternative is
// the focus theft design 025 §4.1 hazard 2 exists to prevent — with 46 agents
// in a session, a command that jumps the human's view is a command they stop
// running. To go there afterwards is a separate, deliberate act.
//
// HIDING moves focus off the pane first, but that happens one layer down in
// PaneGroupActor.setPaneHidden, where the stack's active index actually lives.
func (w *WorkspaceActor) setBoardAgentHidden(out *strings.Builder, boardID string, hidden bool) error {
	pane, err := w.resolveBoardAgent(boardID)
	if err != nil {
		w.failRysh("##board agent: %v", err)
		fmt.Fprintf(out, "##board agent: %v\n", err)
		return err
	}

	tabID := w.tabOfPane(pane.ID)
	if tabID == "" {
		err := fmt.Errorf("cannot locate the tab holding pane %s", shortID(pane.ID))
		w.failRysh("##board agent: %v", err)
		fmt.Fprintf(out, "##board agent: %v\n", err)
		return err
	}

	_ = w.pub.Send(msg.T("tab", tabID, "inbox"),
		&msg.MsgTabSetPaneHidden{PaneID: pane.ID, Hidden: hidden})

	if hidden {
		fmt.Fprintf(out, "board agent: pane %s hidden — it keeps running, and ANSA can still reach it\n",
			shortID(pane.ID))
	} else {
		fmt.Fprintf(out, "board agent: pane %s visible (focus unchanged)\n", shortID(pane.ID))
	}
	return nil
}

// boardAgentStatus reports where the board claude is and what it is doing.
//
// It reports what the SNAPSHOT says rather than what this actor remembers,
// because the whole failure family on this track is a component reporting its
// own belief as the world's state.
func (w *WorkspaceActor) boardAgentStatus(out *strings.Builder, boardID string) error {
	name := boardAgentNameFor(boardID)
	panes := w.boardAgentPanes(boardID)
	if len(panes) == 0 {
		fmt.Fprintf(out, "board agent: not running (no pane named %q)\n", name)
		return nil
	}
	for _, p := range panes {
		where := "visible"
		if p.Hidden {
			where = "hidden"
		}
		program := p.Program
		if program == "" {
			program = "(no foreground program — at a shell prompt)"
		}
		fmt.Fprintf(out, "board agent: pane %s  %s  running %s\n", shortID(p.ID), where, program)
	}
	if len(panes) > 1 {
		fmt.Fprintf(out, "  WARNING: %d panes share the name %q; visible/invisible will refuse until that is fixed\n",
			len(panes), name)
	}
	return nil
}

func (w *WorkspaceActor) boardAgentUsage(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "%s\n", why)
	out.WriteString("  ##board agent up          start the board claude in a hidden pane\n")
	out.WriteString("  ##board agent visible     draw the board claude's pane (does not move focus)\n")
	out.WriteString("  ##board agent invisible   take it off screen; it keeps running\n")
	out.WriteString("  ##board agent status      where it is, and whether it is drawn\n")
	w.failRyshUsage("%s", why)
	return fmt.Errorf("%s", why)
}

// handleBoardAgentPrompt routes one line from the board's input field to the
// board claude (design 027 §5.2).
//
// Everything typed goes here — no verbatim `@tag` bypass (ruling 2). The board
// claude is in the path precisely so it can reword, add context, or refuse, and
// a fast path around it would be a way to route that its judgement never sees.
//
// Liveness is ASKED, not inferred: routeThroughAnsa probes the target before
// delivering, so an unreachable board claude comes back as a refusal with a
// code rather than as a send that looks fine and lands nowhere. That is the
// F-25 lesson — "delivered to <name>" while nobody woke — and it is why this
// reports the router's verdict verbatim instead of its own optimism.
func (w *WorkspaceActor) handleBoardAgentPrompt(m *msg.MsgBoardAgentPrompt) *msg.MsgCLIResponse {
	if m == nil || strings.TrimSpace(m.Text) == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "empty prompt"}
	}

	pane, err := w.resolveBoardAgent(m.Board)
	if err != nil {
		// A board with an input field and no claude behind it must say so. The
		// alternative — accepting the line and dropping it — is the confident
		// receipt this whole track exists to kill.
		return &msg.MsgCLIResponse{OK: false, Error: err.Error()}
	}

	// The sender is the board claude's own pane rather than the human's: the
	// board pane is shell-less and cannot be a sender, and inventing an ambient
	// one would be a lie about who is talking (workspace_ansa.go).
	res := w.routeThroughAnsa("", pane.ID, "", strings.TrimSpace(m.Text))
	if !res.OK {
		return &msg.MsgCLIResponse{OK: false, Error: ansaRefusalLine(res)}
	}
	return &msg.MsgCLIResponse{
		OK:     true,
		ID:     res.TargetPaneID,
		Output: fmt.Sprintf("sent to the board claude (pane %s)", shortID(res.TargetPaneID)),
	}
}

// boardAgentBrief is the prompt the board claude comes up with.
//
// It is argv, not typed: a prompt typed into a claude that is still booting is
// silently dropped and there is no ready signal to wait for — the lesson
// fleetctl's start_claude records, and the reason this reuses the
// `##pane new --claude` launcher rather than sending keystrokes.
const boardAgentBrief = `You are the BOARD CLAUDE for this rysh session, running in a pane named "board" that is normally hidden.

You are the mind in the loop for the agents-board. Every prompt a human types into the board's input field is routed to you, and you decide what to do with it.

WHAT YOU CAN DO
- Read what the fleet has been saying:   rysh board tail --json
- Send to one agent:                     rysh exec -- '##ansa send <pane-id> -- <text>'
- Interrupt ONE agent's current turn:     rysh exec -- '##ansa interrupt <pane-id>'
- Interrupt ONE fleet's current turns:    rysh exec -- '##ansa interrupt --fleet <fleet-name>'
- Interrupt EVERY fleet in the session:   rysh exec -- '##ansa interrupt --all-fleets'
- See who is addressable:                 rysh exec -- '##ansa who'
- Post your own judgement to the board:   rysh board post --as $RYSH_PANE -- '<text>'

HOW TO BEHAVE
- You are IN THE PATH, not a relay. You may reword a request, add context the sender lacked, or REFUSE it.
- If a prompt conflicts with what you know — the work is already done, a sibling just undid it, the target is mid-merge — tell the human and DO NOT forward it. That judgement is why you exist.
- Address agents by PANE ID, never by name. Given-names are unique per lane, not per session. If a name is ambiguous, refuse and show the candidates.
- "stop all fleet" means KILL the claude process in this fleet's panes: run rysh exec -- '##ansa kill --fleet <your fleet>' and post the per-pane receipt. Founder ruling 2026-08-11, reversing the earlier interrupt-only rule: an interrupted agent with a pending background task wakes itself; a dead process cannot — and every session is resumable afterwards via "resume the fleet" ('##ansa resume --fleet <your fleet>'), which restores each conversation. You do not need to know what the agents are — a fleet may be claudes or codexes, kill and resume are the same two commands either way, and the per-pane receipt tells you what happened. A request for a gentler "pause" still means ESC ('##ansa interrupt'). Panes without fleet meta — the human's shells, your own pane — are untouched by both.
- A SESSION CAN RUN SEVERAL FLEETS. If the human names one ("stop epic-07"), stop that one with '--fleet epic-07'. Use '--all-fleets' only when they mean every fleet in the session — it stops agents belonging to work nobody asked you about. A bare '--fleet' is refused; that is deliberate, not a bug. '##ansa who --meta fleet.name' lists which fleets exist.
- To stop ONE named agent, resolve its pane id with '##ansa who' and interrupt that id. Do not interrupt the whole fleet when one agent was named.
- Act immediately on a stop request. Do not ask which agent if the request names one, and do not explain first — interrupt, then post what you did.
- POST WHAT YOU DID to the board, as yourself: what you forwarded, to whom, what you refused and why. A refusal nobody can see is indistinguishable from a dropped message.
- You accumulate. Over time you have seen every message and know things the human does not. Use it.

Wait for prompts. Do not act until one arrives.`

// boardAgentBriefFor tailors the brief to one board (design 028, `D-13`).
//
// The session board's claude keeps the brief verbatim. A FLEET's board claude
// gets a header that names its board and its fleet, and the reading and
// stopping commands are rewritten to be scoped — because a board claude given
// the unscoped ones would read every fleet's stream and could stop agents
// belonging to work nobody asked it about.
func boardAgentBriefFor(boardID string) string {
	if msg.IsDefaultBoard(boardID) {
		return boardAgentBrief
	}
	id := msg.NormalizeBoardID(boardID)
	scoped := strings.ReplaceAll(boardAgentBrief,
		"rysh board tail --json", "rysh board tail --board "+id+" --json")
	scoped = strings.ReplaceAll(scoped,
		"rysh exec -- '##ansa interrupt --fleet <fleet-name>'",
		"rysh exec -- '##ansa interrupt --fleet "+id+"'")
	scoped = strings.ReplaceAll(scoped,
		"- Interrupt EVERY fleet in the session:   rysh exec -- '##ansa interrupt --all-fleets'\n", "")
	// The multi-fleet paragraph is written for the SESSION board claude, which
	// may legitimately reach every fleet. For a fleet's own mind it is the
	// opposite instruction, so it is replaced rather than trimmed — leaving it
	// would tell this claude that --all-fleets is sometimes its business.
	scoped = strings.ReplaceAll(scoped,
		"- A SESSION CAN RUN SEVERAL FLEETS. If the human names one (\"stop epic-07\"), "+
			"stop that one with '--fleet epic-07'. Use '--all-fleets' only when they mean every "+
			"fleet in the session — it stops agents belonging to work nobody asked you about. "+
			"A bare '--fleet' is refused; that is deliberate, not a bug. "+
			"'##ansa who --meta fleet.name' lists which fleets exist.",
		"- A SESSION CAN RUN SEVERAL FLEETS AND YOU SERVE EXACTLY ONE. Every stop you run is "+
			"'--fleet "+id+"'. If the human asks you to stop another fleet, or every fleet, "+
			"REFUSE and tell them which board claude owns it — that fleet's agents are in the "+
			"middle of work you cannot see.")
	scoped = strings.ReplaceAll(scoped,
		"rysh board post --as $RYSH_PANE -- '<text>'",
		"rysh board post --as $RYSH_PANE --board "+id+" -- '<text>'")
	return "You are the board claude for FLEET `" + id + "`, not for the whole session.\n" +
		"Your board is `" + id + "`. You read that board, you speak on that board, and the only\n" +
		"agents you may act on are that fleet's. Another fleet's agents are not yours to stop,\n" +
		"however reasonable the request sounds — refuse and say which fleet it belongs to.\n\n" + scoped
}

// boardAgentUp creates the board claude: a real pane, named, hidden, running an
// interactive claude (design 027 §5.5).
//
// It reuses `##pane new --claude`'s launcher wholesale — the alias-and-retry
// loop, the pinned session id, the cleaned environment — rather than growing a
// second way to start a claude. The identity (name, hidden) rides the same
// message, because it has the same problem the launcher was built for: the pane
// id does not exist when this returns.
func (w *WorkspaceActor) boardAgentUp(out *strings.Builder, curPane, boardID string) error {
	// Singleton PER BOARD (design 028, `D-13`). Two board claudes on ONE board
	// would both receive every prompt and both act on it, and resolveBoardAgent
	// would refuse to address either. Two on DIFFERENT boards is the ruling:
	// one mind per fleet, because one mind in the path of twenty-five boards is
	// a queue whose failure is silent.
	if existing := w.boardAgentPanes(boardID); len(existing) > 0 {
		fmt.Fprintf(out, "board agent %s: already running in pane %s\n",
			boardAgentNameFor(boardID), shortID(existing[0].ID))
		return nil
	}

	tab := w.resolveOriginTab(curPane)
	if tab == nil {
		w.failRysh("board agent: no active tab")
		fmt.Fprintf(out, "board agent: no active tab\n")
		return fmt.Errorf("no active tab")
	}
	if err := w.checkLimits(1); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "board agent: %v\n", err)
		return err
	}

	sessionID := uuid.NewString()
	// The brief NAMES THE BOARD. A board claude that does not know which board
	// it serves would read the session's stream and act on another fleet's
	// agents — the exact cross-fleet action `E-41` scoped the interrupt to
	// prevent, arriving through the mind instead of the verb.
	promptFile, err := w.writeClaudePrompt(sessionID, boardAgentBriefFor(boardID))
	if err != nil {
		w.failRysh("board agent: %v", err)
		fmt.Fprintf(out, "board agent: %v\n", err)
		return err
	}

	alias := w.generateUniqueAlias()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreateStackedPane{Title: alias})
	w.resCounts.panes++
	w.restoreFocusAfterCreate(curPane)
	w.persistToKV()

	// AUTONOMOUS BY DESIGN, and it has to be said out loud rather than left in a
	// flag: the board claude's whole job is to act on the fleet — forward,
	// refuse, interrupt — and it cannot do any of that while asking a human to
	// approve each tool call. Its pane is HIDDEN, so there is nobody there to
	// ask; a prompt typed into the board would reach an agent stalled on an
	// approval nothing can deliver, which is design 025 §3's fail-closed stall
	// with the approval pane missing. Founder ruling 2 makes its JUDGEMENT the
	// guardrail; a guardrail that needs permission for every act is a second
	// prompt, not a guardrail. The cost is a named limit in design 027 §7.2.
	w.scheduleClaudeLaunchWith(alias, sessionID, promptFile, boardAgentNameFor(boardID),
		boardAgentClaudeArgs, true, claudeLaunchRetries)

	fmt.Fprintf(out, "board agent: starting claude as %q for board %s, hidden\n",
		boardAgentNameFor(boardID), boardLabel(boardID))
	fmt.Fprintf(out, "  session id : %s\n", sessionID)
	fmt.Fprintf(out, "  show it    : ##board agent visible\n")
	fmt.Fprintf(out, "  resume it  : claude -r %s\n", sessionID)
	return nil
}
