// SPDX-License-Identifier: Apache-2.0

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
			// Meta is COPIED, not aliased — see ansaPane.Meta. This transport
			// reads tab snapshots the workspace actor owns and may rewrite.
			out = append(out, ansaPane{
				ID:        p.ID,
				GivenName: p.GivenName,
				Title:     p.Title,
				Meta:      ansaCopyMeta(p.Meta),
			})
		}
	}
	return out, nil
}

func (t *workspaceAnsaTransport) Probe(paneID string) (string, error) {
	reply, err := t.w.pub.Request(
		msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true},
		ansaProbeTimeout,
	)
	if err != nil {
		return "", err
	}
	snap, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		// It answered, so it is alive. An unrecognised reply costs the program,
		// not the liveness verdict.
		return "", nil
	}
	return snap.Snapshot.Program, nil
}

func (t *workspaceAnsaTransport) Deliver(paneID, mode, text, program string) error {
	if err := ansaDeliverToInbox(t.w.pub, paneID, mode, text, program); err != nil {
		return err
	}
	// One CR is not reliably enough for a wrapped work order — see
	// ansaConfirmSubmitted. Both transports verify, or the door a human uses
	// behaves differently from the door an agent uses.
	return ansaConfirmSubmitted(t.w.pub, t.Screen, paneID, text, program)
}

func (t *workspaceAnsaTransport) Interrupt(paneID string) error {
	return ansaInterruptPane(t.w.pub, paneID)
}

func (t *workspaceAnsaTransport) Kill(paneID string, hard bool) error {
	return t.w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneKillForeground{PaneID: paneID, Hard: hard})
}

// ansaTurnSubject is the pane's own agent — one subject for all three native
// verbs, because status, cancel and continue are three questions for the same
// actor and a second address for one of them is how F-23 happened.
func ansaTurnSubject(paneID string) string {
	return msg.T("pane", paneID, "llm_prompt_execution", "inbox")
}

// TurnStatus asks the pane's executor what it is doing. A timeout is reported
// as "nothing running", not as an error: every pane has an executor, so the
// only pane that cannot answer is one with nothing to say, and turning that
// into a failure would make an idle shell look like a stop that went wrong.
func (t *workspaceAnsaTransport) TurnStatus(paneID string) (bool, bool, error) {
	return ansaTurnStatus(t.w.pub, paneID)
}

func (t *workspaceAnsaTransport) CancelTurn(paneID string) error {
	return t.w.pub.Send(ansaTurnSubject(paneID), &msg.MsgAgenticCancel{})
}

func (t *workspaceAnsaTransport) ContinueTurn(paneID string) error {
	return t.w.pub.Send(ansaTurnSubject(paneID), &msg.MsgAgenticContinue{})
}

func (t *workspaceAnsaTransport) ArmAutoApprove(paneID string) error {
	return ansaArmAutoApprove(t.w.pub, paneID)
}

// Screen reads the pane's live VT rows, over the same request the TUI and the
// web server already use for this (`("pane", id, "snapshot")` carrying
// MsgGetPaneVT). One subject, one message type, three callers — a second way to
// ask the same question is how F-23 happened.
func (t *workspaceAnsaTransport) Screen(paneID string) ([]string, error) {
	reply, err := t.w.pub.Request(
		msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneVT{},
		ansaProbeTimeout,
	)
	if err != nil {
		return nil, err
	}
	vt, ok := reply.(*msg.MsgPaneVTReply)
	if !ok {
		return nil, fmt.Errorf("unexpected reply %T from the pane VT read", reply)
	}
	return vt.Screen, nil
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
		return w.ansaWho(out, args[1:])

	case "interrupt":
		// ##ansa interrupt <@name|pane-id> | --fleet
		return w.ansaInterrupt(out, args[1:])

	case "kill":
		// ##ansa kill --fleet <name> | --all-fleets — the HARD stop: signal
		// the claude process in each fleet pane and verify by process state.
		return w.ansaKill(out, args[1:])

	case "resume":
		// ##ansa resume --fleet <name> | --all-fleets — restart each killed
		// claude with `claude --resume <session-id>`, delivered over the
		// inbox: the pane is a bare shell after a kill, so nothing is typed.
		return w.ansaResume(out, args[1:])

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

// ansaInterrupt is `##ansa interrupt <@name|pane-id>` or
// `##ansa interrupt --fleet`.
//
// It cancels the TURN in progress. It never signals and never kills, so an
// interrupted agent keeps its context, its transcript and its worktree and
// stays resumable — founder ruling 3. See ansaInterruptPane for the mechanism
// and ansaFleetPanes for why `--fleet` cannot reach the human's own shells.
//
// `--fleet` NOW REQUIRES A NAME (design 028 §6.6, `E-41`). It used to be a bare
// flag meaning "every pane carrying fleet.role", which was the whole session's
// agents — correct while a session ran one fleet, and a session-wide stop the
// moment it runs two. Founder ruling 8 (027) chose that blast radius knowingly
// for ONE fleet; inheriting it silently for twenty-five is not the same
// decision, so the bare flag is refused and the wide form has to be typed:
// `--all-fleets`.
//
// Refusing rather than defaulting is the point. Either default is wrong in a
// way the operator cannot see: "all" stops twenty-four fleets nobody asked
// about, and "mine" guesses which fleet the caller meant.
func (w *WorkspaceActor) ansaInterrupt(out *strings.Builder, args []string) error {
	fleet := ""
	allFleets := false
	bareFleetFlag := false
	target := ""
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		switch {
		case a == "":
			continue
		case a == "--all-fleets":
			allFleets = true
		case a == "--fleet":
			// The value is the next word, if there is one that is not itself a
			// flag. Anything else is the bare form and is refused below.
			if i+1 < len(args) && !strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
				fleet = strings.TrimSpace(args[i+1])
				i++
				continue
			}
			bareFleetFlag = true
		case strings.HasPrefix(a, "--fleet="):
			fleet = strings.TrimSpace(strings.TrimPrefix(a, "--fleet="))
			if fleet == "" {
				bareFleetFlag = true
			}
		case strings.HasPrefix(a, "-"):
			return w.ansaUsage(out, fmt.Sprintf("unknown flag %q to ##ansa interrupt", a))
		default:
			if target != "" {
				// Refused rather than "interrupt the first one": on this verb a
				// misparse is not a wasted command, it is an agent losing a turn
				// it was in the middle of.
				return w.ansaUsage(out, "##ansa interrupt takes ONE target, or a fleet")
			}
			target = a
		}
	}

	scoped := fleet != ""
	switch {
	case bareFleetFlag:
		return w.ansaUsage(out, "##ansa interrupt --fleet needs a fleet NAME (`--fleet <name>`). "+
			"A session can run several fleets, so a bare --fleet would stop every agent in all of "+
			"them; type --all-fleets if that is what you mean")
	case allFleets && (scoped || target != ""):
		return w.ansaUsage(out, "##ansa interrupt takes --all-fleets alone")
	case scoped && target != "":
		return w.ansaUsage(out, "##ansa interrupt takes a target OR a fleet, never both")
	case !allFleets && !scoped && target == "":
		return w.ansaUsage(out, "##ansa interrupt needs a pane id, an @given-name, "+
			"--fleet <name>, or --all-fleets")
	case allFleets:
		return w.ansaInterruptFleet(out, "")
	case scoped:
		return w.ansaInterruptFleet(out, fleet)
	}
	return w.ansaInterruptOne(out, target)
}

func (w *WorkspaceActor) ansaInterruptOne(out *strings.Builder, target string) error {
	r := w.ansaRouterFor()

	panes, err := r.tr.Panes()
	if err != nil {
		refusal := msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so %q can be neither found nor ruled out: %v",
			target, err)
		return w.ansaInterruptRefused(out, refusal)
	}

	// THE EDGE, and the same one `send` uses — not a second resolver. An
	// ambiguous name is refused with the candidate ids and NOTHING is
	// interrupted; guessing here would cancel a turn belonging to an agent the
	// caller never named.
	p, refusal := ansaResolveTarget(panes, target)
	if refusal != nil {
		return w.ansaInterruptRefused(out, refusal)
	}

	res := r.Interrupt(p.ID)
	if !res.OK {
		return w.ansaInterruptRefused(out, res)
	}
	fmt.Fprintf(out, "interrupt sent to %s (pane %s)\n", res.TargetPersona, res.TargetPaneID)
	out.WriteString(ansaInterruptQueueCaution)
	return nil
}

// fleet is the fleet to stop, or "" for every fleet.
func (w *WorkspaceActor) ansaInterruptFleet(out *strings.Builder, fleet string) error {
	r := w.ansaRouterFor()

	outcomes, refusal := r.InterruptFleet(fleet)
	if refusal != nil {
		return w.ansaInterruptRefused(out, refusal)
	}

	failed := 0
	for _, o := range outcomes {
		if o.Err != nil {
			failed++
			fmt.Fprintf(out, "  FAILED  %s  %s [%s]: %v\n", o.PaneID, o.Persona, o.Role, o.Err)
			continue
		}
		fmt.Fprintf(out, "  %s  %s  %s [%s]\n", ansaDrainLabel(o.Drain), o.PaneID, o.Persona, o.Role)
	}
	// Say what was sent, not what was interrupted. Nothing acknowledges a raw
	// key, so "interrupted 12 agents" would be a claim this code cannot make.
	//
	// It also says WHICH fleet and by which key, because with several fleets in
	// a session "12 fleet panes" does not tell an operator whether the right
	// twelve were reached.
	//
	// ONE Fprintf, with the scope computed first, rather than a line per branch:
	// TestEveryInterruptReceiptStatesTheQueueLimit counts receipts against
	// cautions in this file's source, and a second receipt string inside one
	// door would make that count wrong while the behaviour was fine. Keeping one
	// receipt per door is what keeps that guard able to catch a THIRD door added
	// later without its caution — which is the defect it exists for.
	scope := fmt.Sprintf("EVERY fleet (selected by %s)", ansaMetaFleetRole)
	if fleet != "" {
		scope = fmt.Sprintf("fleet %s (selected by %s + %s=%s)",
			fleet, ansaMetaFleetRole, ansaMetaFleetName, fleet)
	}
	fmt.Fprintf(out, "interrupt sent to %d of %d panes in %s\n",
		len(outcomes)-failed, len(outcomes), scope)
	out.WriteString(ansaInterruptQueueCaution)
	if failed > 0 {
		w.failRysh("%d of %d fleet interrupts failed to publish", failed, len(outcomes))
		return fmt.Errorf("%d of %d fleet interrupts failed to publish", failed, len(outcomes))
	}
	return nil
}

func (w *WorkspaceActor) ansaInterruptRefused(out *strings.Builder, res *msg.MsgAnsaRouteResult) error {
	fmt.Fprintf(out, "##ansa interrupt: %s\n", ansaRefusalLine(res))
	for _, id := range res.Candidates {
		fmt.Fprintf(out, "    %s\n", id)
	}
	w.failRysh("%s", ansaRefusalLine(res))
	return fmt.Errorf("%s", ansaRefusalLine(res))
}

// ansaWho lists addressable panes and FLAGS DUPLICATE NAMES, so a name that
// cannot be used as an address is visible before somebody tries to use it
// rather than only in the refusal afterwards.
func (w *WorkspaceActor) ansaWho(out *strings.Builder, args []string) error {
	filter, ferr := parseAnsaWhoArgs(args)
	if ferr != nil {
		return w.ansaUsage(out, ferr.Error())
	}

	r := w.ansaRouterFor()
	panes, err := r.tr.Panes()
	if err != nil {
		w.failRysh("cannot enumerate panes: %v", err)
		fmt.Fprintf(out, "##ansa who: cannot enumerate panes: %v\n", err)
		return fmt.Errorf("cannot enumerate panes: %w", err)
	}

	// Ambiguity is counted over ALL panes, not over the filtered set. A name
	// that is ambiguous in the session is ambiguous whether or not the other
	// holder matched the filter — reporting otherwise would tell a caller a
	// name is safe to address when it is not.
	counts := map[string]int{}
	for _, p := range panes {
		if p.GivenName != "" {
			counts[p.GivenName]++
		}
	}

	shown := 0
	for _, p := range panes {
		if !filter.matches(p) {
			continue
		}
		shown++
		name := ansaPersona(p)
		marker := ""
		if p.GivenName != "" && counts[p.GivenName] > 1 {
			marker = "  [AMBIGUOUS — address this one by id]"
		}
		fmt.Fprintf(out, "  %s  %s%s%s\n", p.ID, name, ansaRoleTag(p), marker)
	}

	switch {
	case len(panes) == 0:
		out.WriteString("  (no panes)\n")
	case shown == 0:
		// Say WHY nothing is listed. An empty roster under a filter is a
		// different fact from an empty session, and a caller that cannot tell
		// them apart concludes the session is gone.
		fmt.Fprintf(out, "  (no pane matches %s — %d panes in the session)\n", filter, len(panes))
	case filter.active():
		fmt.Fprintf(out, "  (%d of %d panes match %s)\n", shown, len(panes), filter)
	}
	return nil
}

// ansaRoleTag renders a pane's fleet role beside its name, and nothing at all
// for a pane that has none.
//
// Nothing, rather than a placeholder: a pane with no fleet meta is a
// first-class citizen of this session (founder ruling A), and printing "[-]"
// beside it would make a non-fleet claude look like a degraded fleet one.
func ansaRoleTag(p ansaPane) string {
	if role := p.fleetRole(); role != "" {
		return "  [" + role + "]"
	}
	return ""
}

// ansaMetaFilter is a parsed `--meta` argument: a key that must be present, and
// optionally the value it must have.
type ansaMetaFilter struct {
	key      string
	value    string
	hasValue bool
}

func (f ansaMetaFilter) active() bool { return f.key != "" }

func (f ansaMetaFilter) String() string {
	if !f.active() {
		return "any pane"
	}
	if f.hasValue {
		return "--meta " + f.key + "=" + f.value
	}
	return "--meta " + f.key
}

// matches reports whether a pane satisfies the filter.
//
// A key that is ABSENT and a key that is PRESENT-BUT-EMPTY are treated the
// same, and deliberately: fleetctl writes `fleet.role` on every pane it
// creates, so an empty value means the writer had nothing to say rather than
// that the pane holds an empty role. Selecting on it would put a pane in a
// fleet operation on the strength of a blank.
func (f ansaMetaFilter) matches(p ansaPane) bool {
	if !f.active() {
		return true
	}
	v := strings.TrimSpace(p.meta(f.key))
	if v == "" {
		return false
	}
	if f.hasValue {
		return v == f.value
	}
	return true
}

// parseAnsaMetaFilter parses `key` or `key=value`.
func parseAnsaMetaFilter(arg string) (ansaMetaFilter, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ansaMetaFilter{}, fmt.Errorf(
			"--meta needs a key, e.g. --meta %s or --meta %s=worker", ansaMetaFleetRole, ansaMetaFleetRole)
	}
	if i := strings.Index(arg, "="); i >= 0 {
		key := strings.TrimSpace(arg[:i])
		if key == "" {
			return ansaMetaFilter{}, fmt.Errorf("--meta %q has no key before the '='", arg)
		}
		return ansaMetaFilter{key: key, value: strings.TrimSpace(arg[i+1:]), hasValue: true}, nil
	}
	return ansaMetaFilter{key: arg}, nil
}

// parseAnsaWhoArgs parses `##ansa who [--meta <key>[=<value>]]`.
//
// An unrecognised argument is REFUSED rather than ignored. A silently dropped
// `--met fleet.role` would list every pane in the session while the caller
// believed it had asked for the fleet — and on this surface that answer is the
// input to a decision about who to interrupt.
func parseAnsaWhoArgs(args []string) (ansaMetaFilter, error) {
	var f ansaMetaFilter
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		switch {
		case a == "":
			continue
		case a == "--meta":
			if i+1 >= len(args) {
				return f, fmt.Errorf(
					"--meta needs a key, e.g. --meta %s or --meta %s=worker",
					ansaMetaFleetRole, ansaMetaFleetRole)
			}
			parsed, err := parseAnsaMetaFilter(args[i+1])
			if err != nil {
				return f, err
			}
			f = parsed
			i++
		case strings.HasPrefix(a, "--meta="):
			parsed, err := parseAnsaMetaFilter(strings.TrimPrefix(a, "--meta="))
			if err != nil {
				return f, err
			}
			f = parsed
		default:
			return f, fmt.Errorf("unknown argument %q to ##ansa who", a)
		}
	}
	return f, nil
}

func (w *WorkspaceActor) ansaUsage(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "%s\n", why)
	out.WriteString("  ##ansa send <@name|pane-id> <text>    deliver a shell line to another pane\n")
	out.WriteString("  ##ansa prompt <@name|pane-id> <text>  deliver a prompt to another pane\n")
	out.WriteString("  ##ansa who [--meta <key>[=<value>]]   list addressable panes and their ids;\n")
	out.WriteString("                                        --meta fleet.role selects fleet panes only\n")
	out.WriteString("  ##ansa interrupt <@name|pane-id>      cancel the TURN in that pane (ESC, never a\n")
	out.WriteString("                                        signal) — the session stays alive/resumable\n")
	out.WriteString("  ##ansa interrupt --fleet <name>       same, for every pane in THAT fleet\n")
	out.WriteString("                                        (fleet.role + fleet.name meta)\n")
	out.WriteString("  ##ansa interrupt --all-fleets         same, for every pane carrying fleet.role meta,\n")
	out.WriteString("                                        in every fleet; panes without it are spared\n")
	out.WriteString("                                        by construction\n")
	out.WriteString("  ##ansa kill --fleet <name>|--all-fleets    HARD stop: signal each fleet pane's claude\n")
	out.WriteString("                                        and VERIFY dead by process state — nothing\n")
	out.WriteString("                                        can re-wake a dead process (F-41); sessions\n")
	out.WriteString("                                        stay resumable by their pinned id\n")
	out.WriteString("  ##ansa resume --fleet <name>|--all-fleets  bring killed claudes back with\n")
	out.WriteString("                                        claude --resume <session-id>, same args\n")
	w.failRyshUsage("%s", why)
	return fmt.Errorf("%s", why)
}

// ansaDrainLabel says, per pane, what the drained stop actually achieved.
//
// Four states and they are not collapsible. "stopped" means a screen was read
// and showed no work. "unread" means the pane never answered, so nothing is
// known — reporting that as stopped is the receipt-without-delivery this whole
// track exists to kill. "+N" records how many extra interrupts the queue cost,
// which is the only visible evidence that a queue existed at all.
func ansaDrainLabel(d ansaDrainOutcome) string {
	switch {
	case d.Unreadable != nil:
		return "unread "
	case !d.Quiet:
		return "WORKING"
	case d.Rewoke > 0:
		// The pane woke itself after being put down — a background task
		// completing, or a sibling's in-flight delivery — and the re-sweep put
		// it down again. The headline is the wake, not the drain arithmetic:
		// this is the pane an operator must expect to wake AGAIN if it still
		// holds a pending task.
		return fmt.Sprintf("rewoke+%d", d.Rewoke)
	case d.Rounds == 0:
		return "stopped"
	default:
		return fmt.Sprintf("drained+%d", d.Rounds)
	}
}
