// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##board attach <pane>` — the workspace half (design 030 §3, §9.4; W5,
// E-46; founder ruling D-23 "Never"; security-lead early notes, root
// dd54b9944).
//
// It turns an existing idle shell pane into the pane that shows a board. The
// conversion ends that pane's shell, so everything here is about refusing:
// the workspace refuses what it can see (who is asking, what the target is,
// whether the board already has a pane), then asks the pane, and the pane makes
// the final busy check in the same turn as the teardown
// (pane_board_attach.go). No --force exists (D-23).

// boardAttachWait is how long the workspace waits for the pane's answer. The
// request carries the same deadline, so a pane that sees it late refuses
// instead of converting unrecorded.
const boardAttachWait = 2 * time.Second

// errBoardAttachRefused marks a refusal that has already been printed.
var errBoardAttachRefused = errors.New("board attach refused")

// withScheduledFire marks the workspace as running a scheduled fire (cron or
// trigger) for the length of fn. The workspace actor is single-threaded and
// the fire's routing is synchronous, so a field is exact: nothing else runs
// while it is set.
func (w *WorkspaceActor) withScheduledFire(label string, fn func()) {
	prev := w.scheduledFire
	w.scheduledFire = label
	defer func() { w.scheduledFire = prev }()
	fn()
}

func (w *WorkspaceActor) attachBoardToPane(out *strings.Builder, callerPane, boardID string, args []string) error {
	boardID = msg.NormalizeBoardID(boardID)
	target := ""
	if len(args) == 1 {
		target = strings.TrimSpace(args[0])
	}

	refuse := func(why string) error {
		line := fmt.Sprintf("board %s: attach refused: %s", boardLabel(boardID), why)
		fmt.Fprintf(out, "%s\n", line)
		w.failRysh("%s", line)
		w.logBoardAttach(callerPane, target, boardID, "refused: "+why)
		return fmt.Errorf("%w: %s", errBoardAttachRefused, why)
	}

	if target == "" {
		return refuse("attach needs exactly one pane (id, alias or given name): ##board attach <pane> [--board <id>]")
	}
	// A scheduled fire has no human at the keyboard, and its "caller" is just
	// the pane it was aimed at (security-lead note 1).
	if w.scheduledFire != "" {
		return refuse(fmt.Sprintf("not from a scheduled %s fire: attach ends a shell, so a person types it", w.scheduledFire))
	}
	// Only a line a PERSON typed (TUI keyboard, web UI). Every program route
	// reaches the dispatcher without inputTyped: rysh exec / script / send /
	// prompt (Programmatic submits or MsgCLIRyshCommand), an agent's rysh tool
	// (Programmatic), a control-share controller and remote-serve
	// (MsgExecRyshOnPane), rysh run (Programmatic). On those routes the caller
	// pane is self-asserted or borrowed from the human's focus, so the caller
	// checks below could not tell an agent from a person (security-lead
	// review). The web UI is a person's door, gated like every other ## command.
	if !w.inputTyped {
		return refuse("only a person typing in a pane (keyboard or web UI) may attach a board; " +
			"this line came from a program (rysh exec/send/prompt, an agent's tool, a share controller or a script)")
	}

	// One registry read serves the caller and the target. Silent → refuse:
	// "is this pane a fleet member?" has no safe default (W3's rule).
	members, err := w.boardAttachFleetMembers()
	if err != nil {
		return refuse("the fleet registry did not answer, so fleet membership cannot be checked")
	}

	// The caller must be a pane we can see: a line from no pane, or from a
	// pane id that names nothing, has no caller to check, so it fails closed.
	cs := (*domain.PaneSnapshot)(nil)
	if callerPane != "" {
		cs = w.paneSnapshotByID(callerPane)
	}
	if cs == nil {
		return refuse("the caller is not a known pane; type ##board attach in a pane")
	}
	if key := busyMetaKey(cs.Meta); key != "" {
		return refuse(fmt.Sprintf("the caller pane has %s meta; agents and fleet panes may not attach boards", key))
	}
	if f := members[callerPane]; f != "" {
		return refuse(fmt.Sprintf("the caller pane is a member of fleet %s; agents and fleet panes may not attach boards", f))
	}

	paneID := w.resolvePaneID(target)
	if paneID == "" {
		return refuse(fmt.Sprintf("no pane named %q", target))
	}
	ps := w.paneSnapshotByID(paneID)
	if ps == nil {
		return refuse(fmt.Sprintf("no pane named %q", target))
	}
	label := paneDisplayName(ps)

	if existing := w.boardPaneFor(boardID); existing != "" {
		tabAlias, paneAlias := w.boardAttachWhere(existing)
		return refuse(fmt.Sprintf("already attached in tab %s pane %s — detach it there first", tabAlias, paneAlias))
	}
	if ps.PaneType != domain.PaneTypeNormal {
		return refuse(fmt.Sprintf("pane %s is not a shell pane (type %s)", label, ps.PaneType))
	}
	if key := busyMetaKey(ps.Meta); key != "" {
		return refuse(fmt.Sprintf("pane %s is busy: it has %s meta (an agent or fleet pane)", label, key))
	}
	if f := members[paneID]; f != "" {
		return refuse(fmt.Sprintf("pane %s is busy: it is a member of fleet %s", label, f))
	}

	ask := w.boardAttachAsk
	if ask == nil {
		ask = w.defaultBoardAttachAsk
	}
	reason, err := ask(paneID, boardID, time.Now().Add(boardAttachWait))
	if err != nil {
		return refuse(fmt.Sprintf("pane %s did not answer (%v); nothing changed", label, err))
	}
	if reason != "" {
		return refuse(fmt.Sprintf("pane %s: %s", label, reason))
	}

	if w.boardPanes == nil {
		w.boardPanes = map[string]string{}
	}
	w.boardPanes[boardID] = paneID
	w.announceExistingPanesToBoard(boardID)
	w.persistToKV()
	w.logBoardAttach(callerPane, paneID, boardID, "attached")
	fmt.Fprintf(out, "board %s: attached to pane %s (%s); its shell has ended. "+
		"Background jobs it started get a hangup and usually end (ones started with nohup, disown or setsid keep running).\n",
		boardLabel(boardID), shortID(paneID), label)
	return nil
}

// busyMetaKey names the agent or fleet meta that makes a pane busy (D-23 (b)),
// or "".
func busyMetaKey(meta map[string]string) string {
	for _, key := range []string{metaAgentNative, metaAgentKind, ansaMetaFleetRole} {
		if meta[key] != "" {
			return key
		}
	}
	return ""
}

func paneDisplayName(ps *domain.PaneSnapshot) string {
	if ps.Title != "" {
		return ps.Title
	}
	if ps.GivenName != "" {
		return ps.GivenName
	}
	return shortID(ps.ID)
}

// boardAttachFleetMembers maps every fleet member's pane id to its fleet.
func (w *WorkspaceActor) boardAttachFleetMembers() (map[string]string, error) {
	if w.nc == nil {
		return nil, errors.New("no bus")
	}
	reply, err := fleet.Ask(w.nc, fleet.Query{}, fleetBoardDoorTimeout)
	if err != nil {
		return nil, err
	}
	members := map[string]string{}
	for _, f := range reply.Fleets {
		for _, m := range f.Members {
			if m.PaneID != "" {
				members[m.PaneID] = f.Name
			}
		}
	}
	return members, nil
}

// boardAttachWhere names the tab and pane a board is already shown in.
func (w *WorkspaceActor) boardAttachWhere(paneID string) (tabAlias, paneAlias string) {
	tabAlias, paneAlias = "?", shortID(paneID)
	for _, t := range w.tabs {
		if t == nil {
			continue
		}
		ts := w.queryTabSnapshot(t.id)
		if ts == nil {
			continue
		}
		if p := domain.FindPaneInTab(ts, paneID); p != nil {
			tabAlias = t.title
			if tabAlias == "" {
				tabAlias = shortID(t.id)
			}
			return tabAlias, paneDisplayName(p)
		}
	}
	return tabAlias, paneAlias
}

// defaultBoardAttachAsk sends the request to the pane and waits for its answer.
func (w *WorkspaceActor) defaultBoardAttachAsk(paneID, boardID string, deadline time.Time) (string, error) {
	reply, err := w.pub.Request(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneAttachBoard{BoardID: boardID, DeadlineUnixMs: deadline.UnixMilli()},
		time.Until(deadline))
	if err != nil {
		return "", err
	}
	r, ok := reply.(*msg.MsgPaneAttachBoardReply)
	if !ok {
		return "", fmt.Errorf("unexpected reply %T", reply)
	}
	return r.Reason, nil
}

// logBoardAttach writes one daemon-log line per attempt (security-lead note
// 5): who asked, which pane, which board, the result. Never pane content.
func (w *WorkspaceActor) logBoardAttach(caller, target, boardID, result string) {
	slog.Info("board attach", "caller", caller, "target", target, "board", boardID,
		"time", time.Now().UTC().Format(time.RFC3339), "result", result)
}
