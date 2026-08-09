package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The two doors onto ANSA, and — as with the board — the difference between
// them is the point.
//
//   - handleCLIAnsaSend is the AGENT door (msg.MsgCLIAnsaSend, `rysh ansa
//     send`). Silent: it focuses nothing and writes into no pane's buffers.
//   - handleAnsaCommand is the HUMAN door (`##ansa`, typed into a pane). It
//     rides the normal ## path, so it echoes into the caller's own pane — which
//     is wanted, because someone is looking at that pane.
//
// Both resolve @name to a pane id HERE, at the edge, and hand the router an id.
// That is the founder's addressing rule: a name never travels as an address.
//
// WHY THESE DOORS DO NOT ROUND-TRIP THROUGH THE ANSA ACTOR. They build a router
// over a workspace-local transport instead of sending MsgAnsaRoute to
// T("ansa","inbox"). The ANSA actor's own transport enumerates panes by
// requesting T("ws","snapshot") — which this very actor serves. A workspace
// blocked in its Receive waiting on ANSA, while ANSA waits on the workspace for
// a snapshot, is a deadlock. Same policy, same refusals, different directory
// lookup; the ANSA actor remains the door for every caller that is NOT the
// workspace.

// workspaceAnsaTransport enumerates panes from the workspace's own tab
// snapshots — the path the board's handler already uses — and delivers over the
// same pane inbox as the actor's transport.
type workspaceAnsaTransport struct {
	w *WorkspaceActor
}

func (t *workspaceAnsaTransport) Panes() ([]ansaPane, error) {
	var out []ansaPane
	for _, info := range t.w.tabs {
		tabSnap := t.w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			// A tab that cannot answer is NOT an empty tab. Reporting it as one
			// would turn "I could not look" into "it is not there", and the
			// caller would be told its target does not exist when the truth is
			// that the session is under load. That distinction is the whole
			// reason AnsaErrDirectory is a separate code.
			return nil, fmt.Errorf("tab %s did not answer a snapshot request", info.id)
		}
		for p := range domain.PanesInTab(tabSnap) {
			out = append(out, ansaPane{ID: p.ID, GivenName: p.GivenName, Title: p.Title})
		}
	}
	return out, nil
}

func (t *workspaceAnsaTransport) Probe(paneID string) error {
	_, err := t.w.pub.Request(
		msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true},
		ansaProbeTimeout,
	)
	return err
}

func (t *workspaceAnsaTransport) Deliver(paneID, mode, text string) error {
	return ansaDeliverToInbox(t.w.pub, paneID, mode, text)
}

// ansaRouterFor builds the workspace-local router.
func (w *WorkspaceActor) ansaRouterFor() *ansaRouter {
	return &ansaRouter{tr: &workspaceAnsaTransport{w: w}}
}

// routeThroughAnsa is the shared body of both doors: resolve the target at the
// edge, then route BY ID.
func (w *WorkspaceActor) routeThroughAnsa(from, to, mode, text string) *msg.MsgAnsaRouteResult {
	r := w.ansaRouterFor()

	panes, err := r.tr.Panes()
	if err != nil {
		return msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so %q can be neither found nor ruled out: %v", to, err)
	}

	// THE EDGE. @name becomes a pane uuid here and nowhere deeper.
	target, refusal := ansaResolveTarget(panes, to)
	if refusal != nil {
		return refusal
	}

	return r.Route(msg.NewAnsaRoute(from, target.ID, mode, text))
}

// ---------------------------------------------------------------------------
// the agent door
// ---------------------------------------------------------------------------

// handleCLIAnsaSend routes on behalf of an agent. It focuses nothing and writes
// into no pane's buffers — see the three hazards in workspace_board.go, all of
// which apply harder here: a control channel carrying work orders between
// dozens of agents must not yank the human's cursor around as a side effect of
// agents talking to each other.
func (w *WorkspaceActor) handleCLIAnsaSend(m *msg.MsgCLIAnsaSend) *msg.MsgCLIResponse {
	if m == nil {
		return &msg.MsgCLIResponse{OK: false, Error: "empty ansa send"}
	}

	// The SENDER is optional and is never inferred from the active pane: a
	// wrong "from" is a lie about who is talking, and routing from outside a
	// pane (cron, a script, a test) is legitimate.
	from := strings.TrimSpace(m.AsPaneID)

	res := w.routeThroughAnsa(from, m.To, m.Mode, m.Text)
	if !res.OK {
		return &msg.MsgCLIResponse{OK: false, Error: ansaRefusalLine(res)}
	}
	return &msg.MsgCLIResponse{
		OK:     true,
		ID:     res.TargetPaneID,
		Output: fmt.Sprintf("delivered to %s (pane %s)\n", res.TargetPersona, res.TargetPaneID),
	}
}

// ansaRefusalLine renders a refusal for a human or a log, code included. The
// code is kept in the text as well as in the typed result because the CLI
// response only carries a string — a caller reading stderr must still be able
// to tell "ambiguous" from "gone".
func ansaRefusalLine(res *msg.MsgAnsaRouteResult) string {
	if res.Code == "" {
		return res.Error
	}
	return fmt.Sprintf("[%s] %s", res.Code, res.Error)
}

// ---------------------------------------------------------------------------
// the human door
// ---------------------------------------------------------------------------

// handleAnsaCommand is `##ansa`, typed by a human into their own pane.
func (w *WorkspaceActor) handleAnsaCommand(out *strings.Builder, paneID string, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	switch sub {
	case "send":
		// ##ansa send <@name|pane-id> <text>
		if len(args) < 3 {
			return w.ansaUsage(out, "##ansa send needs a target and some text")
		}
		target := args[1]
		text := strings.TrimSpace(strings.Join(args[2:], " "))
		if text == "" {
			return w.ansaUsage(out, "##ansa send needs some text")
		}
		return w.ansaSendFromPane(out, paneID, target, msg.AnsaModeShell, text)

	case "prompt":
		// ##ansa prompt <@name|pane-id> <text> — the same route, delivered as a
		// prompt rather than a shell line. A separate subcommand rather than a
		// --mode flag because the two do very different things to the target.
		if len(args) < 3 {
			return w.ansaUsage(out, "##ansa prompt needs a target and some text")
		}
		target := args[1]
		text := strings.TrimSpace(strings.Join(args[2:], " "))
		if text == "" {
			return w.ansaUsage(out, "##ansa prompt needs some text")
		}
		return w.ansaSendFromPane(out, paneID, target, msg.AnsaModePrompt, text)

	case "who":
		// The roster, and the reason it is here: an ambiguity refusal tells you
		// to re-address by id, and this is where you get the ids.
		return w.ansaWho(out)

	default:
		if sub == "" {
			return w.ansaUsage(out, "##ansa needs a subcommand")
		}
		return w.ansaUsage(out, fmt.Sprintf("unknown ##ansa subcommand: %q", sub))
	}
}

func (w *WorkspaceActor) ansaSendFromPane(out *strings.Builder, paneID, target, mode, text string) error {
	res := w.routeThroughAnsa(paneID, target, mode, text)
	if !res.OK {
		fmt.Fprintf(out, "##ansa: %s\n", ansaRefusalLine(res))
		// An ambiguity refusal is only useful if the human can see the choices.
		for _, id := range res.Candidates {
			fmt.Fprintf(out, "    %s\n", id)
		}
		w.failRysh("%s", ansaRefusalLine(res))
		return fmt.Errorf("%s", ansaRefusalLine(res))
	}
	fmt.Fprintf(out, "delivered to %s (pane %s)\n", res.TargetPersona, res.TargetPaneID)
	return nil
}

// ansaWho lists addressable panes and FLAGS DUPLICATE NAMES, so a name that
// cannot be used as an address is visible before somebody tries to use it
// rather than only in the refusal afterwards.
func (w *WorkspaceActor) ansaWho(out *strings.Builder) error {
	r := w.ansaRouterFor()
	panes, err := r.tr.Panes()
	if err != nil {
		w.failRysh("cannot enumerate panes: %v", err)
		fmt.Fprintf(out, "##ansa who: cannot enumerate panes: %v\n", err)
		return fmt.Errorf("cannot enumerate panes: %w", err)
	}

	counts := map[string]int{}
	for _, p := range panes {
		if p.GivenName != "" {
			counts[p.GivenName]++
		}
	}

	for _, p := range panes {
		name := ansaPersona(p)
		marker := ""
		if p.GivenName != "" && counts[p.GivenName] > 1 {
			marker = "  [AMBIGUOUS — address this one by id]"
		}
		fmt.Fprintf(out, "  %s  %s%s\n", p.ID, name, marker)
	}
	if len(panes) == 0 {
		out.WriteString("  (no panes)\n")
	}
	return nil
}

func (w *WorkspaceActor) ansaUsage(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "%s\n", why)
	out.WriteString("  ##ansa send <@name|pane-id> <text>    deliver a shell line to another pane\n")
	out.WriteString("  ##ansa prompt <@name|pane-id> <text>  deliver a prompt to another pane\n")
	out.WriteString("  ##ansa who                            list addressable panes and their ids\n")
	w.failRyshUsage("%s", why)
	return fmt.Errorf("%s", why)
}
