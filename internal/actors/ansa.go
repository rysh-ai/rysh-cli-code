// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ANSA — the Agent Nervous System Actor. One per SESSION, always on, a router.
//
// An agent names a target; ANSA delivers to that pane's inbox and TELLS THE
// CALLER WHAT HAPPENED. That last clause is the entire reason this actor
// exists, because the transport underneath it cannot do it: the per-pane inbox
// already works, but `pub.Send(T("pane", id, "inbox"), x)` returns nil when the
// pane id is a typo, when the pane died an hour ago, and when the payload is a
// type the PaneActor's Receive has no case for. Three different ways to lose a
// message, all of them reported as success.
//
// The CEO's rule, verbatim: *a board post is worth less than the control
// channel; a work order is worth more than either.* So ANSA falls back or fails
// loudly and never silently drops. Every refusal carries a machine-readable
// msg.AnsaErr* code.
//
// WHAT ANSA IS NOT. It is not new transport and it is not a second bus. It is a
// mediator in front of the inbox that already exists, and it holds no queue, no
// retry and no state — a router, not a broker.

// ---------------------------------------------------------------------------
// what the router needs from a running session
// ---------------------------------------------------------------------------

// ansaPane is one addressable pane, flattened out of whatever snapshot the
// transport used to find it.
type ansaPane struct {
	ID        string
	GivenName string
	Title     string

	// Meta is the pane's free-form metadata, copied out of the snapshot.
	//
	// IT IS A COPY, NEVER AN ALIAS. A router holding a live reference into a
	// snapshot map is a data race waiting for -race to find it: the snapshot
	// belongs to the actor that produced it and may be rewritten while this
	// router reads it. ansaCopyMeta is the only way this field is populated.
	//
	// ALWAYS NON-NIL, so a caller can index it without a guard. A pane with no
	// metadata carries an empty map, not nil.
	Meta map[string]string
}

// Fleet metadata keys, stamped by .claude/skills/rysh-fleet/fleetctl.py when it
// creates a pane. ANSA does not own them and does not validate them — fleet is
// metadata on a pane, never a boundary on this actor (founder ruling A) — but
// it must be able to READ them, because that is the only way `--fleet` can tell
// an agent's pane from the human's shell.
const (
	ansaMetaFleetRole = "fleet.role"
	ansaMetaFleetName = "fleet.name"
	ansaMetaFleetUnit = "fleet.unit"
)

// ansaCopyMeta copies a snapshot's metadata map for the router to hold.
//
// Returns a non-nil empty map for an absent or empty source, so nothing
// downstream has to nil-check before reading. Reading a nil map is legal in Go;
// the reason to normalise anyway is that this map gets RANGED and compared, and
// a mixture of nil and empty is the sort of difference that produces a test
// that passes for the wrong reason.
func ansaCopyMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// meta reads one metadata key. Safe on a zero ansaPane.
func (p ansaPane) meta(key string) string {
	if p.Meta == nil {
		return ""
	}
	return p.Meta[key]
}

// fleetRole is the pane's role in a fleet, or "" for every pane that is not one
// — the human's shells, a roadmap supervisor, a non-fleet claude.
//
// THE EMPTY STRING IS LOAD-BEARING. `##ansa interrupt --fleet` selects on this
// being non-empty, so a pane is spared by the ABSENCE of metadata rather than
// by appearing on an exclusion list. That fails safe: if fleet meta is never
// populated, --fleet interrupts NOTHING. An exclusion list fails the other way
// — anything nobody remembered to exclude gets hit.
func (p ansaPane) fleetRole() string {
	return strings.TrimSpace(p.meta(ansaMetaFleetRole))
}

// fleetName is the pane's fleet, or "" for a pane in no fleet.
//
// It is what makes `--fleet <name>` mean ONE fleet (design 028 §6.6, `E-41`).
// Role alone was enough while a session held one fleet; with several, selecting
// on role is "every agent in the session", so a scoped stop needs both — role
// says "this is an agent", name says "this is MY agent".
//
// A pane carrying a role but NO name is in no fleet this command can address,
// and is therefore never selected by a named `--fleet`. That is the same
// fail-closed argument as the empty role: a pane is included by carrying the
// metadata, never by failing to carry an exclusion.
func (p ansaPane) fleetName() string {
	return strings.TrimSpace(p.meta(ansaMetaFleetName))
}

// ansaTransport is the seam between routing POLICY (which target, which
// refusal) and routing MECHANISM (NATS subjects, snapshots, timeouts).
//
// It exists so the failure paths can be tested for real. Every refusal below is
// a branch that only fires when the session misbehaves — a dead pane, a
// duplicated given-name, a transport that says no — and none of those can be
// provoked reliably against a live daemon. With this seam a fake transport
// produces each one on demand, which is why the never-silently-drop property
// has tests at all rather than a comment claiming it holds.
type ansaTransport interface {
	// Panes enumerates every addressable pane in the session. An error here is
	// "I could not look", which is NOT the same as "it is not there".
	Panes() ([]ansaPane, error)
	// Probe reports whether a pane is alive and pumping its mailbox, and
	// returns the pane's live FOREGROUND program ("claude", "vim", "" for a
	// bare shell). The program comes back from the SAME round trip on purpose:
	// how a message must be delivered is a fact about the target, and F-23 was
	// caused by one fact having two sources.
	Probe(paneID string) (program string, err error)
	// Deliver publishes the message the target can actually receive. program is
	// what Probe saw; see ansaDeliverToInbox for why it decides everything.
	Deliver(paneID, mode, text, program string) error
	// Interrupt sends the target's interrupt gesture. It is NOT Deliver with a
	// special payload — see ansaInterruptPane.
	Interrupt(paneID string) error
	// Screen returns the target's live VT rows, or an error if it cannot be
	// read. It exists for ONE caller — the drain loop below — because a stop
	// that cannot be verified is the thing F-30 was.
	Screen(paneID string) ([]string, error)
	// Kill signals the pane's foreground process group: SIGTERM, or SIGKILL
	// when hard. The pane guards its own shell, so a kill aimed at a pane
	// whose program already exited is a no-op rather than a pane-killer.
	Kill(paneID string, hard bool) error

	// TurnStatus asks the pane's OWN agent — rysh's LLMPromptExecutionActor —
	// whether it is mid-turn and whether it is paused.
	//
	// This is the native counterpart of Probe. Probe answers "what program owns
	// the PTY", which is the whole truth for a claude or a codex and NOT THE
	// TRUTH AT ALL for a rysh agent: a native agent runs inside the daemon, so
	// its pane has no foreground program while it is working as hard as any
	// subprocess. A stop verb that reads only Probe therefore looks at a
	// working rysh agent and sees an idle shell.
	//
	// A pane that does not answer is not a native agent mid-turn — every pane
	// has an executor, so silence here means nothing is running in it. That is
	// the safe reading: it costs a no-op, never a missed stop.
	TurnStatus(paneID string) (inFlight, paused bool, err error)
	// CancelTurn cancels the in-flight native turn with PAUSE semantics: the
	// orchestrator's context is cancelled and the conversation is checkpointed,
	// so ContinueTurn can pick it up. It is NOT a signal and NOT ESC.
	CancelTurn(paneID string) error
	// ContinueTurn resumes a paused native turn from its checkpoint.
	ContinueTurn(paneID string) error
	// ArmAutoApprove puts a native agent back into approval-free mode.
	//
	// It exists because CANCELLING A TURN DISARMS THE RUN BUDGET, and
	// auto-approval rides on that budget: an agent launched approval-free comes
	// back from a fleet stop GATED, and stalls at its next tool call on a prompt
	// nobody is watching. That is f8824f5 — a resumed agent in the wrong
	// permission mode — in native dress, and resume owns the fix on both sides.
	ArmAutoApprove(paneID string) error
}

// ansaRouter is the policy half: pure decisions over an ansaTransport.
//
// THE INVARIANT THIS TYPE ENFORCES: the router only ever holds an ID. A name
// never travels as an address, and nothing below this line performs a name
// lookup. @name is resolved at the EDGE — in the command that accepted the @ —
// and the router receives the pane uuid that came out of it.
//
// This is the wave-1 board finding one layer down. Given-names are unique per
// LANE, not per session (TabActor.IsGivenNameTakenInLane), so a name is a label
// for humans and an id is an address. If ANSA internals ever hold a name where
// an id belongs, that is the bug — not the absence of uniqueness enforcement.
type ansaRouter struct {
	tr ansaTransport
}

// Route delivers to a pane addressed BY ID, checks it is alive first, and
// answers. Exactly one success path; every other branch is a coded refusal, and
// it never returns nil.
func (r *ansaRouter) Route(req *msg.MsgAnsaRoute) *msg.MsgAnsaRouteResult {
	if req == nil {
		return msg.AnsaRefusal(msg.AnsaErrNoTarget, "empty route request")
	}

	to := strings.TrimSpace(req.To)
	if to == "" {
		return msg.AnsaRefusal(msg.AnsaErrNoTarget, "no target: name a pane id")
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return msg.AnsaRefusal(msg.AnsaErrNoText, "nothing to deliver: the message text is empty")
	}

	// Validate the mode BEFORE touching the session. A bad mode is the caller's
	// mistake and must be reported as such rather than defaulted into a shell
	// command — delivering a prompt as a shell line is not a near miss, it runs
	// somebody's sentence in bash.
	mode, err := ansaNormaliseMode(req.Mode)
	if err != nil {
		return msg.AnsaRefusal(msg.AnsaErrBadMode, "%v", err)
	}

	panes, err := r.tr.Panes()
	if err != nil {
		return msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so %q can be neither found nor ruled out: %v", to, err)
	}

	target, found := ansaPaneByID(panes, to)
	if !found {
		// A name arriving here is a caller that skipped edge resolution, and it
		// gets its own code rather than a vague "unknown". Silently resolving it
		// would put a name where an id belongs, which is the one thing this
		// router must never do — and would quietly re-open the ambiguity hole
		// the edge resolver closes.
		if ansaNameExists(panes, to) {
			return msg.AnsaRefusal(msg.AnsaErrNotAnID,
				"%q is a pane NAME, not a pane id — ansa addresses by pane uuid on the wire; "+
					"resolve @name at the edge and send the id", to)
		}
		return msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
			"no pane with id %q in this session", to)
	}

	// Liveness probe before the publish. The inbox is fire-and-forget, so this
	// is the strongest confirmation available without inventing an ack
	// protocol: a pane that answers a request/reply is alive and pumping its
	// mailbox. Without it, a message to a pane that died is indistinguishable
	// from a delivered one.
	program, err := r.tr.Probe(target.ID)
	if err != nil {
		return msg.AnsaRefusal(msg.AnsaErrUnreachable,
			"pane %s (%s) did not answer — it may have exited; nothing was delivered: %v",
			target.ID, ansaPersona(target), err)
	}

	// A WORK ORDER TO A RYSH AGENT ARMS ITS APPROVALS FIRST, every time — not
	// only at launch.
	//
	// The run budget is disarmed on EVERY terminal outcome, a clean finish
	// included, and auto-approval used to ride only on that budget. So a fleet
	// agent launched approval-free was approval-free for exactly ONE TURN: its
	// second work order stopped at the first `bash` on `[y]es [Y]es always
	// [n]o` and waited for a human who is not coming. The fleet reads as alive
	// and delivers nothing, which is the shape this whole track exists to kill.
	//
	// The arm is now PERSISTENT (MsgSetRunBudget.AutoApprovePersist, F-56):
	// it sets the actor-level flag disarm never touches, so once any arm has
	// landed the agent stays approval-free across turns. This per-prompt
	// re-arm is kept as the belt: it is what covers an agent whose actor was
	// respawned by a daemon restart, and panes armed by an older binary whose
	// arm was still per-run.
	//
	// Armed here rather than at the executor's terminal branch because THIS is
	// where the agent's permission mode is known: the pane carries the stamp
	// fleetctl wrote, and a pane without it is one somebody deliberately gated.
	// Best-effort — a failed arm must not swallow the order, since a gated
	// delivery is still a delivery and the receipt says what was sent.
	if mode == msg.AnsaModePrompt && program == "" &&
		strings.TrimSpace(target.meta(ansaMetaAutoApprove)) == "true" {
		_ = r.tr.ArmAutoApprove(target.ID)
	}

	if err := r.tr.Deliver(target.ID, mode, text, program); err != nil {
		return msg.AnsaRefusal(msg.AnsaErrPublishFailed,
			"delivery to pane %s failed, nothing was sent: %v", target.ID, err)
	}

	return &msg.MsgAnsaRouteResult{
		OK:            true,
		TargetPaneID:  target.ID,
		TargetPersona: ansaPersona(target),
	}
}

// ansaNormaliseMode defaults an empty mode and rejects anything else. Empty →
// shell matches the pane inbox's own default (see PaneSendInput), so the
// default is inherited rather than invented.
func ansaNormaliseMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "":
		return msg.AnsaModeShell, nil
	case msg.AnsaModeShell:
		return msg.AnsaModeShell, nil
	case msg.AnsaModePrompt:
		return msg.AnsaModePrompt, nil
	default:
		return "", fmt.Errorf("unknown mode %q: use %q or %q",
			mode, msg.AnsaModeShell, msg.AnsaModePrompt)
	}
}

// ansaPaneByID is the router's only lookup: an exact id match, nothing else.
func ansaPaneByID(panes []ansaPane, id string) (ansaPane, bool) {
	for _, p := range panes {
		if p.ID == id {
			return p, true
		}
	}
	return ansaPane{}, false
}

// ansaNameExists reports whether a string is some pane's name — used ONLY to
// give a caller that sent a name instead of an id a precise error. It resolves
// nothing.
func ansaNameExists(panes []ansaPane, ref string) bool {
	for _, p := range panes {
		if (p.GivenName != "" && p.GivenName == ref) || (p.Title != "" && p.Title == ref) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// the EDGE resolver — the only place a name becomes an address
// ---------------------------------------------------------------------------

// ansaResolveTarget turns what a human or an agent typed into a pane ID.
//
// This is the edge, and it is the ONLY place in ANSA where a name is looked up.
// Everything downstream — the request on the wire, the router, the transport —
// carries the uuid it returns. A leading "@" is accepted and stripped, because
// @name is how people write it and refusing the sigil would just move the
// stripping somewhere less careful.
//
// Resolution order, and each rank is deliberate:
//
//  1. exact pane id — unambiguous by construction, and it must win outright so
//     that "re-address by id" remains a working escape hatch even when some
//     other pane carries that string as a name;
//  2. given-name — what a human assigned;
//  3. auto-title — generated, so it ranks below an explicit name and can never
//     shadow one.
//
// AMBIGUITY IS A HARD ERROR. Two panes may legally share a given-name, so a
// first-match "helpful guess" would hand a work order to the wrong agent and
// report success — the worst outcome available to this code. Refusing is always
// correct; guessing is never. The refusal carries the candidate ids so the
// sender can choose, which is what makes it a usable error rather than a wall.
func ansaResolveTarget(panes []ansaPane, ref string) (ansaPane, *msg.MsgAnsaRouteResult) {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "@")
	if ref == "" {
		return ansaPane{}, msg.AnsaRefusal(msg.AnsaErrNoTarget,
			"no target: name a pane id or @given-name")
	}

	if p, ok := ansaPaneByID(panes, ref); ok {
		return p, nil
	}

	matches := ansaMatchByName(panes, ref)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return ansaPane{}, msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
			"no pane matches %q — address it by full pane id or by @given-name", ref)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		res := msg.AnsaRefusal(msg.AnsaErrAmbiguousTarget,
			"%q matches %d panes (%s) — given-names are unique per LANE, not per session, "+
				"so this name is not an address; re-send to one of those ids",
			ref, len(ids), strings.Join(ids, ", "))
		res.Candidates = ids
		return ansaPane{}, res
	}
}

// ansaMatchByName collects every pane a name matches, given-names first. It
// returns ALL of them — collapsing to one is exactly the bug.
func ansaMatchByName(panes []ansaPane, ref string) []ansaPane {
	var byName []ansaPane
	for _, p := range panes {
		if p.GivenName != "" && p.GivenName == ref {
			byName = append(byName, p)
		}
	}
	if len(byName) > 0 {
		return byName
	}
	var byTitle []ansaPane
	for _, p := range panes {
		if p.Title != "" && p.Title == ref {
			byTitle = append(byTitle, p)
		}
	}
	return byTitle
}

// ansaPersona is the display name for a resolved pane. It reuses the board's
// resolver so the two subsystems can never disagree about what an agent is
// called — including the guard against approval panes, which overload
// GivenName to carry "requestID\x1FresponseSubject".
func ansaPersona(p ansaPane) string {
	return msg.BoardPersona(p.GivenName, p.Title, p.ID)
}

// ---------------------------------------------------------------------------
// the NATS transport
// ---------------------------------------------------------------------------

// ansaProbeTimeout bounds the liveness probe. It is short on purpose: the probe
// is on the path of every route, and a wedged pane must be reported as
// unreachable quickly rather than stalling the caller. A pane that cannot
// answer a layout-only snapshot within this window is not in a state to act on
// a work order either.
const ansaProbeTimeout = 2 * time.Second

// ansaSubmitPause separates the typed text from the CR that submits it. An
// inline TUI's paste detection swallows a newline that arrives in the same
// burst as the text, which leaves the message sitting unsent in the composer —
// see ansaTypeIntoPane.
const ansaSubmitPause = 120 * time.Millisecond

// ansaSnapshotTimeout bounds the directory lookup.
const ansaSnapshotTimeout = 3 * time.Second

// natsAnsaTransport enumerates panes from the workspace snapshot and delivers
// over the existing pane inbox. Used by the ANSA actor.
type natsAnsaTransport struct {
	pub *msg.NATSPublisher
}

func (t *natsAnsaTransport) Panes() ([]ansaPane, error) {
	reply, err := t.pub.Request(
		msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true},
		ansaSnapshotTimeout,
	)
	if err != nil {
		return nil, err
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("unexpected reply %T from the workspace snapshot", reply)
	}
	return ansaPanesFromWorkspace(&snapReply.Snapshot), nil
}

func (t *natsAnsaTransport) Probe(paneID string) (string, error) {
	reply, err := t.pub.Request(
		msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true},
		ansaProbeTimeout,
	)
	if err != nil {
		return "", err
	}
	snap, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		// The pane answered, so it is alive — which is what Probe is for. An
		// unrecognised reply costs us the program, not the liveness verdict.
		return "", nil
	}
	return snap.Snapshot.Program, nil
}

func (t *natsAnsaTransport) Deliver(paneID, mode, text, program string) error {
	if err := ansaDeliverToInbox(t.pub, paneID, mode, text, program); err != nil {
		return err
	}
	return ansaConfirmSubmitted(t.pub, t.Screen, paneID, text, program)
}

func (t *natsAnsaTransport) Interrupt(paneID string) error {
	return ansaInterruptPane(t.pub, paneID)
}

func (t *natsAnsaTransport) Kill(paneID string, hard bool) error {
	return t.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneKillForeground{PaneID: paneID, Hard: hard})
}

// The native trio. Both transports implement them the same way — one subject,
// three questions for the pane's own agent — so a rysh agent behaves
// identically whether ANSA is reached in-process or over the bus.
func (t *natsAnsaTransport) TurnStatus(paneID string) (bool, bool, error) {
	return ansaTurnStatus(t.pub, paneID)
}

func (t *natsAnsaTransport) CancelTurn(paneID string) error {
	return t.pub.Send(ansaTurnSubject(paneID), &msg.MsgAgenticCancel{})
}

func (t *natsAnsaTransport) ContinueTurn(paneID string) error {
	return t.pub.Send(ansaTurnSubject(paneID), &msg.MsgAgenticContinue{})
}

func (t *natsAnsaTransport) ArmAutoApprove(paneID string) error {
	return ansaArmAutoApprove(t.pub, paneID)
}

// ansaArmAutoApprove sends the native equivalent of the CLI agents' permission
// flag. AutoContinue stays FALSE deliberately: an armed auto-continue budget
// re-wakes a paused run on its own, which is the one thing a stop verb must
// never leave behind.
//
// AutoApprovePersist makes the grant ACTOR-level rather than run-level, so it
// survives the disarm that fires on every terminal outcome (clean finishes
// included). Without it a fleet agent was approval-free for exactly one turn
// and its second work order stalled at the first `bash` on an approval prompt
// nobody was watching — the per-prompt re-arm in ansaRoute still exists as a
// belt for panes armed by an older binary, but the stamp it reads is pane meta,
// which a daemon restart can wipe (F-54); the persistent grant does not depend
// on it.
func ansaArmAutoApprove(pub *msg.NATSPublisher, paneID string) error {
	return pub.Send(ansaTurnSubject(paneID),
		&msg.MsgSetRunBudget{AutoContinue: false, AutoApprove: true, AutoApprovePersist: true})
}

// ansaTurnStatusTimeout bounds the one question the stop verb asks every
// program-less pane. Short on purpose: it is paid per pane on every kill, and
// an executor that is alive answers from its own fields with no I/O.
const ansaTurnStatusTimeout = 900 * time.Millisecond

// ansaTurnStatus asks a pane's agent whether it is mid-turn and whether it is
// paused. Silence is "nothing running", never an error — see the interface.
func ansaTurnStatus(pub *msg.NATSPublisher, paneID string) (bool, bool, error) {
	reply, err := pub.Request(ansaTurnSubject(paneID), &msg.MsgGetRunStatus{}, ansaTurnStatusTimeout)
	if err != nil {
		return false, false, nil
	}
	st, ok := reply.(*msg.MsgRunStatusReply)
	if !ok {
		return false, false, nil
	}
	return st.InFlight, st.Paused, nil
}

func (t *natsAnsaTransport) Screen(paneID string) ([]string, error) {
	reply, err := t.pub.Request(
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

// ansaDeliverToInbox publishes the message the target can actually RECEIVE.
//
// There are two completely different kinds of target and F-25 is what it cost to
// treat them as one.
//
//  1. A pane with NO foreground program is rysh's own: MsgPaneExecPrompt drives
//     the LLMActor, MsgPaneExecShell runs a command. This is the original path
//     and it is correct for those panes.
//
//  2. A pane RUNNING A PROGRAM (claude, vim, …) owns the PTY. MsgPaneExecPrompt
//     goes to rysh's LLM and NEVER REACHES that program — so a work order to a
//     claude agent was published successfully, reported "delivered", and the
//     agent sat waiting forever. Every hop of a five-agent fleet died this way.
//     Such a pane is reached the way a human reaches it: by TYPING.
//
// The typing half is not new knowledge — .claude/skills/rysh-fanout's op_send
// has done it for months and documents the trap: rysh types the text and the
// Enter can be absorbed by the TUI's paste detection, so "the prompt then sits
// in the composer forever". The CR is therefore written as its OWN message
// after the text, never appended to it.
//
// Why this matters beyond ANSA: W4 proposed replacing fleetctl's typing path
// with ANSA. Before this fix that swap would have produced a fleet where every
// work order arrives, no agent ever acts, and every send reports ok.
func ansaDeliverToInbox(pub *msg.NATSPublisher, paneID, mode, text, program string) error {
	if program != "" {
		return ansaTypeIntoPane(pub, paneID, text)
	}
	subject := msg.T("pane", paneID, "inbox")
	if mode == msg.AnsaModePrompt {
		return pub.Send(subject, &msg.MsgPaneExecPrompt{Prompt: text})
	}
	return pub.Send(subject, &msg.MsgPaneExecShell{Command: text})
}

// ansaTypeIntoPane delivers to a pane running a foreground program by writing
// raw bytes to its PTY, exactly as a human at the keyboard would.
//
// The text and the CR are two separate publishes with a pause between them.
// That is the whole point and it is load-bearing: an inline TUI treats a burst
// containing a newline as a PASTE and keeps it in the composer, so a CR bundled
// with the text submits nothing and the message sits on screen unsent — visible,
// which is what makes it look delivered.
func ansaTypeIntoPane(pub *msg.NATSPublisher, paneID, text string) error {
	subject := msg.T("pane", paneID, "rawinput")
	// CLEAR THE COMPOSER FIRST. Typing appends to whatever is already on that
	// line, and F-31 is what that costs: a drained stop (F-30) leaves the
	// cancelled order's text in the composer, so the next work order arrived
	// fused to it with no separator —
	//
	//   Reply with exactly the word QUEUED and nothing else.[FLEET demo | WORK …
	//
	// and the agent obeyed the prefix, answered it, and never opened the order.
	// That is worse than a lost message because it ANSWERS: a `collect` waits,
	// gets a prompt plausible reply, and concludes the work was done.
	//
	// Cleared HERE rather than in the drain, because the drain is only one way
	// the line ends up dirty — a half-typed human keystroke or an earlier
	// delivery that was never submitted leaves the same residue, and a fix in
	// the drain would leave every other path exposed.
	//
	// ctrl+u, chosen by testing it against a live claude composer rather than
	// by reasoning about readline: it emptied a composer holding two
	// concatenated messages, and is a no-op on an empty one.
	// REPEATED, and the repetition is the fix, not belt-and-braces. ctrl+u
	// discards ONE line. A fleet pane in a four-lane layout is 48 columns, so
	// any work order longer than about forty characters wraps — which is all of
	// them — and a single ctrl+u then clears one wrapped row and leaves the
	// rest. Measured on a live 48-column composer: one ctrl+u changed nothing,
	// five emptied it. The residue is not cosmetic; it either fuses with the
	// delivery (F-31) or swallows the CR so the order is never submitted at all
	// (F-25's shape), and both were observed on this pane.
	//
	// Bounded rather than verified-in-a-loop: each key is one tiny publish and
	// ctrl+u on an empty line is a no-op, so over-sending costs nothing, while
	// reading the screen back per delivery would put a round trip on the hot
	// path of every message the fleet sends. The limit is honest — a composer
	// holding more than ansaClearRepeats lines keeps its tail.
	for i := 0; i < ansaClearRepeats; i++ {
		if err := pub.Send(subject, &msg.MsgRawKeyInput{PaneID: paneID, Data: ansaClearComposerKey}); err != nil {
			return err
		}
		time.Sleep(ansaClearKeyGap)
	}
	time.Sleep(ansaClearPause)
	if err := pub.Send(subject, &msg.MsgRawKeyInput{PaneID: paneID, Data: []byte(text)}); err != nil {
		return err
	}
	time.Sleep(ansaSubmitPause)
	return pub.Send(subject, &msg.MsgRawKeyInput{PaneID: paneID, Data: []byte("\r")})
}

// ---------------------------------------------------------------------------
// interrupt
// ---------------------------------------------------------------------------

// ansaClearComposerKey is ctrl+u — "discard the input line" — sent before every
// typed delivery so a message never fuses with what was already there (F-31).
//
// ansaClearPause lets the target's TUI process the clear before the text
// arrives. Same reasoning as ansaSubmitPause: these are keystrokes to a program
// that redraws, not writes to a buffer.
var (
	ansaClearComposerKey = []byte{0x15}
	ansaClearPause       = 80 * time.Millisecond
	// ansaClearRepeats covers a wrapped composer; ansaClearKeyGap lets the TUI
	// process each one. Eight covers a work order of roughly eight wrapped rows
	// at fleet pane width.
	ansaClearRepeats = 8
	ansaClearKeyGap  = 40 * time.Millisecond
	// ansaSubmitRetries bounds the re-press and ansaSubmitRecheck is the gap
	// between looks. Seconds, not milliseconds: the pane that needs this most is
	// one that is not ready yet — a claude still drawing its splash screen holds
	// the text and ignores the CR — and four presses half a second apart all
	// land inside that same unready window. Measured: two panes tasked during
	// startup kept the order in the composer through a 0.5 s retry budget.
	ansaSubmitRetries = 8

	// ansaSubmitProbeLen is short on purpose: the composer WRAPS, and a probe
	// long enough to straddle a wrap point matches nothing on any single row,
	// which reads as "submitted" for an order still sitting there.
	ansaSubmitProbeLen = 16
)

// ansaInterruptKey is ESC, and ESC is the whole mechanism.
//
// NEVER A SIGNAL, NEVER A KILL — founder ruling 3, and it is not a preference.
// A signal ends the process; ESC ends the TURN. Claude's own affordance is
// "esc to interrupt" (the string .claude/skills/rysh-fanout/scripts/ryshfan.py
// looks for on screen), so an interrupted agent keeps its context, its
// transcript and its worktree, and can simply be told what to do next. A killed
// one has to be rebuilt, and everything it had not written down is gone.
var ansaInterruptKey = []byte{0x1b}

// ansaInterruptPane sends the interrupt gesture to one pane.
//
// INTERRUPT IS NOT DELIVERY, and routing it through ansaDeliverToInbox would be
// the worst available bug on this surface: that path writes the text and then a
// CR, so an "interrupt" would SUBMIT a prompt containing an escape character
// rather than cancelling the turn in progress. One message, one byte, no CR.
//
// It also does not go near MsgPaneExecPrompt/MsgPaneExecShell. A pane running
// claude owns its PTY; rysh's own LLM path never reaches the program (F-25), so
// the only thing that can interrupt a foreground program is what a human at the
// keyboard would send it.
func ansaInterruptPane(pub *msg.NATSPublisher, paneID string) error {
	return pub.Send(msg.T("pane", paneID, "rawinput"), &msg.MsgRawKeyInput{
		PaneID: paneID,
		Data:   ansaInterruptKey,
	})
}

// ansaConfirmSubmitted presses Enter again if the order is still sitting in the
// composer, and it exists because ONE CR is not reliably enough.
//
// Measured on a live 48-column pane: a short prompt submits on the first CR; a
// long one — which is every real work order once it wraps — stays in the
// composer with the CR absorbed. The order is visible on screen, so it looks
// delivered, and ANSA reported `delivered` because the publish succeeded. That
// is F-25's exact shape one layer up, and the fleet only escaped it because
// fleetctl delivers a SHORT pointer ("read <path> in full") rather than a body.
//
// .claude/skills/rysh-fanout's op_send has verified-and-re-pressed for months,
// and the petstore recipe names this gap in so many words: "op_send verifies and
// presses Enter itself; ansa prompt, rysh send --mode prompt and ##cmd do not."
// This closes it for ANSA.
//
// Bounded, and silent about failure BY DESIGN: after ansaSubmitRetries the text
// is still on screen for a human to submit, and returning an error here would
// turn a delivered-but-unsubmitted order into a refusal the caller would retry,
// duplicating it.
func ansaConfirmSubmitted(pub *msg.NATSPublisher, screen func(string) ([]string, error),
	paneID, text, program string) error {
	if program == "" || screen == nil {
		return nil // inbox path: nothing was typed, nothing to submit
	}
	probe := text
	if len(probe) > ansaSubmitProbeLen {
		probe = probe[:ansaSubmitProbeLen]
	}
	for i := 0; i < ansaSubmitRetries; i++ {
		time.Sleep(ansaSubmitRecheck)
		rows, err := screen(paneID)
		if err != nil {
			// A TRANSIENT read failure is not evidence of anything, and
			// treating it as "submitted" is how an order stayed in a composer
			// while every receipt said delivered. The VT request has a 2 s
			// budget and a loaded session misses it; that is exactly when a
			// fleet is being driven hard, so the give-up fired precisely when
			// verification mattered most. Keep looking until the bound.
			continue
		}
		if !ansaComposerHolds(rows, probe) {
			return nil
		}
		if err := pub.Send(msg.T("pane", paneID, "rawinput"),
			&msg.MsgRawKeyInput{PaneID: paneID, Data: []byte("\r")}); err != nil {
			return err
		}
	}
	return nil
}

// ansaComposerHolds reports whether the pane's composer still shows the text we
// typed — i.e. the order was never submitted.
//
// THE COMPOSER IS BETWEEN THE LAST TWO RULES, not after the last one. The first
// version scanned from the final rule to the end of the screen, which is the
// FOOTER ("⏵⏵ bypass permissions on …") and never contains the order. So it
// always answered "not held", ansaConfirmSubmitted always concluded the order
// had been submitted, and the re-press it exists to perform NEVER FIRED — F-35
// shipped as a no-op and the defect it was meant to close stayed open.
//
// Found by filming: three fleet panes sat with the order visible in the
// composer, unsent, while every receipt said delivered. The identical
// off-by-one-region mistake was in the Python harness at the same time, which is
// the tell that the screen's regions need naming once rather than being
// re-derived by eye at each call site.
//
// Rules are the TUI's own horizontal borders. Fewer than two means no composer
// is drawn (a shell pane, or a program that took the alternate screen), and the
// honest answer there is "not held" rather than a guess.
func ansaComposerHolds(rows []string, probe string) bool {
	var rules []int
	for i, r := range rows {
		if strings.Count(r, "─") > 8 {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return false
	}
	open, close := rules[len(rules)-2], rules[len(rules)-1]
	for _, r := range rows[open:close] {
		if strings.Contains(r, probe) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// draining a stop (F-30, founder decision 2026-08-10)
// ---------------------------------------------------------------------------

// ansaDrainRounds bounds how many times a stop re-sends ESC, and
// ansaDrainSettle is how long it waits before looking again.
//
// The bound is the whole safety argument. Draining works by cancelling each
// turn as the target's queue starts it, so an unbounded loop against an agent
// that keeps producing work would never return. Four rounds clears three queued
// orders; past that the receipt says so instead of spinning.
// ansaDrainRounds bounds the EXTRA interrupts; ansaDrainQuietLooks is how many
// CONSECUTIVE quiet reads are required before a pane is called stopped.
//
// The second constant is F-32, and it exists because one quiet reading was
// taken as proof and is not. A cancelled turn does not hand over to its queue
// instantly: the pane is genuinely idle for a moment, and a drain that returns
// on the first quiet read sees exactly that moment, reports `stopped`, and the
// queued order starts behind it — the failure F-30 was raised to remove,
// reintroduced by its own fix. Caught on camera, not by a test, because the
// first live run happened to sample the other side of the gap.
const (
	ansaDrainRounds     = 4
	ansaDrainQuietLooks = 3
)

// ansaDrainSettle is a var, not a const, for one reason: the bounded-loop test
// would otherwise sleep for five real seconds to prove a bound. Tests shrink it;
// nothing else writes it.
var ansaDrainSettle = 1200 * time.Millisecond

// ansaSubmitRecheck is a var so the flake test need not sleep for seconds.
var ansaSubmitRecheck = 1500 * time.Millisecond

// ansaFleetResweeps and ansaResweepSettle bound phase 3 of a fleet stop: how
// many times the whole target set is re-scanned for panes that woke themselves,
// and the pause between scans. Vars so tests need not sleep for real.
//
// The bound is a DOCUMENTED LIMIT, not a fix. A background collect with a
// 15-minute timeout re-wakes its pane 15 minutes from now, and no watch this
// command can afford will be standing there — the command already blocks the
// workspace actor for its whole run. Holding a fleet down for good means either
// killing agent-spawned background processes (a signal — ruling 3 territory,
// the founder's call) or a standing watcher outside this verb.
var (
	ansaFleetResweeps = 3
	ansaResweepSettle = 4 * time.Second
)

// ansaScreenIsWorking reports whether a pane's rendered screen still shows an
// agent mid-turn or holding queued messages.
//
// THIS IS SCREEN MATCHING, AND THIS CODEBASE IS RIGHT TO DISTRUST IT.
// MsgPaneProcess exists precisely so a supervisor can wait on an event instead
// of matching a TUI's footer. But a turn is not a process: claude runs as one
// foreground program across every turn it takes, so process events cannot see a
// turn start or a queue drain, and the screen is the only surface that shows
// either. The alternative is not a cleaner signal — it is no verification, and
// no verification is what made "stop all fleet" report a stop it had not made.
//
// Matched on SHORT prefixes on purpose. A narrow pane truncates the footer:
// at 49 columns "esc to interrupt" renders "esc to interr…", and a full-string
// match there reports a busy pane as idle. That mistake has a name on this
// track — it is what made F-29 look like a defect for a day.
//
// ITS LIMIT IS NOT THEORETICAL AND IT BIT: a fleet pane in a four-lane layout is
// 48 COLUMNS, and at that width claude's footer renders
// "⏵⏵ bypass permissions on (shift+tab to     ·" — "esc to interrupt" is not
// truncated, it is GONE. So this predicate is blind exactly where fleets live,
// which is why F-32's stop reported success over three generating panes. It is
// kept as a FAST PATH for wide panes; the drain no longer trusts it alone. The drain then stops early and the receipt reports
// what it saw, which is why the caller distinguishes "confirmed quiet" from
// "no marker seen" instead of collapsing both into success.
func ansaScreenIsWorking(rows []string) bool {
	for _, r := range rows {
		if strings.Contains(r, "esc to i") || strings.Contains(r, "queued") {
			return true
		}
	}
	return false
}

// ansaDrainOutcome is what a drained stop actually achieved on one pane.
type ansaDrainOutcome struct {
	// Rounds is how many EXTRA interrupts were sent after the first.
	Rounds int
	// Quiet is true only when a screen was read and showed no work. A pane we
	// could not read is not quiet; it is unknown, and the two must not merge.
	Quiet bool
	// Unreadable carries why the screen could not be read, if it could not.
	Unreadable error
	// Rewoke counts the times this pane STARTED WORKING AGAIN after being put
	// down, and was put down again by the re-sweep. A pane can wake itself: a
	// backgrounded Bash task keeps running across turns and re-invokes the
	// session when it exits — the task-notification arrives as a new user turn
	// with no keystroke involved. Proven by transcript 2026-08-11: ESC at
	// 13:45:27Z, task-notification at 13:46:12Z, fresh essay written after the
	// stop.
	Rewoke int
}

// ansaDrainPane re-interrupts a pane until its screen shows no work left.
//
// The first ESC has already been sent by the caller, in the fleet-wide ordering
// that puts the board claude last (§5.3a) — so this loop verifies and tops up,
// it does not initiate. Each round: wait, look, and only interrupt again if the
// pane is working. A pane that was already quiet costs one screen read and no
// extra keystrokes.
func ansaDrainPane(tr ansaTransport, paneID string) ansaDrainOutcome {
	var out ansaDrainOutcome
	quiet := 0
	prev := ""
	// Bounded by construction: every look either ends the loop, spends one of
	// the interrupt rounds, or advances the quiet streak.
	for looks := 0; looks < ansaDrainRounds+ansaDrainQuietLooks+1; looks++ {
		time.Sleep(ansaDrainSettle)
		rows, err := tr.Screen(paneID)
		if err != nil {
			out.Unreadable = err
			return out
		}
		cur := strings.Join(rows, "\n")
		// WIDTH-INDEPENDENT SIGNAL, and the one that actually holds at 48
		// columns: a screen that CHANGED between two reads is a screen
		// something is writing to. The marker match stays as a fast path for
		// panes wide enough to render it, but a narrow pane produces no marker
		// at all and only this comparison can see it working.
		changed := prev != "" && cur != prev
		prev = cur
		if ansaScreenIsWorking(rows) || changed {
			// Any sign of work RESETS the streak. A pane that goes
			// quiet-then-busy was never stopped; it was between orders.
			quiet = 0
			if out.Rounds >= ansaDrainRounds {
				return out
			}
			if err := tr.Interrupt(paneID); err != nil {
				out.Unreadable = err
				return out
			}
			out.Rounds++
			continue
		}
		quiet++
		if quiet >= ansaDrainQuietLooks {
			out.Quiet = true
			return out
		}
	}
	return out
}

// ansaInterruptQueueCaution is printed after every interrupt receipt, and it
// now reports a DRAINED stop rather than warning about an undrained one.
//
// Before the 2026-08-10 founder decision this string existed to admit that ESC
// stops a turn and a queued order starts the next one, so "stop all fleet" left
// the fleet working. The stop now drains (ansaDrainPane), and the honest thing
// to print changed with it: what was cancelled, and what this process still
// cannot promise.
//
// It still refuses to claim a stop it cannot see. Per-pane lines say whether a
// screen was actually read; this line carries the two limits that hold whatever
// the screen said — queued orders are GONE, not deferred, and a pane still
// working after the bound is reported rather than chased forever.
const ansaInterruptQueueCaution = "note: queued orders are CANCELLED, not deferred — a drained stop discards work the fleet had already accepted (F-30). A pane with a PENDING BACKGROUND TASK wakes itself when the task completes; the re-sweep catches wakes inside its window (rewoke+N above) — a longer-running task re-wakes its pane AFTER this receipt, and only re-running the stop puts it down.\n"

// ansaKillOutcome is what a hard stop achieved on one pane, verified by
// PROCESS STATE — the property that makes this verb unlike everything screen-
// read in this file. Probe returns the pane's foreground program on the same
// round trip it always made; "" means the shell is back in front and the
// claude is gone. No pixels are consulted anywhere on this path.
type ansaKillOutcome struct {
	PaneID  string
	Persona string
	Role    string
	// WasRunning is the program Probe saw before the kill ("" = already shell,
	// nothing to do).
	WasRunning string
	// Escalated is true when SIGTERM was verified survived and SIGKILL was
	// sent.
	Escalated bool
	// Dead is true only when a post-kill Probe answered with no foreground
	// program. Unverifiable is not dead.
	Dead bool
	// Native is true when this pane was stopped as a RYSH AGENT — its in-flight
	// turn cancelled inside the daemon — rather than by signalling a process.
	// The receipt must say so: "killed" for something that was never a process
	// is the kind of small lie that makes a whole receipt untrustworthy, and
	// the two outcomes differ in what survives (a cancelled turn keeps its
	// checkpoint; a killed process loses the in-flight response).
	Native bool
	Err    error
}

// ansaKillVerifyRetries / ansaKillVerifyPause bound the wait for a signalled
// group to actually exit before escalating or giving up. Vars for tests.
var (
	ansaKillVerifyRetries = 6
	ansaKillVerifyPause   = 500 * time.Millisecond
)

// killPane stops the claude IN a pane by signal, verifies by process state,
// and escalates once: SIGTERM → probe → SIGKILL → probe.
//
// Founder ruling 2026-08-11, reversing 027 ruling 3 for the stop verb: ESC
// ends a turn but cannot cancel a pending task-notification, so an
// interrupted agent with a background task wakes itself (F-41). A dead
// process cannot be woken. The session id is pinned at launch, so
// `##ansa resume` restores the conversation afterwards.
func (r *ansaRouter) killPane(p ansaPane) ansaKillOutcome {
	out := ansaKillOutcome{PaneID: p.ID, Persona: ansaPersona(p), Role: p.fleetRole()}
	prog, err := r.tr.Probe(p.ID)
	if err != nil {
		out.Err = err
		return out
	}
	out.WasRunning = prog
	if prog == "" {
		// NO FOREGROUND PROGRAM IS NOT THE SAME AS NOTHING RUNNING. This line
		// used to read `Dead = true // already a shell`, and for a rysh agent
		// that was false in the worst available way: a native agent runs inside
		// the daemon, so a pane working flat out on a fifty-step task looks
		// exactly like an idle shell to Probe. The stop reported success, the
		// summary counted the pane as verified dead, and the agent kept going —
		// receipt without delivery, on the one verb built to be free of it.
		return r.killNativeTurn(out, p.ID)
	}
	if err := r.tr.Kill(p.ID, false); err != nil {
		out.Err = err
		return out
	}
	if ansaProbeGone(r.tr, p.ID) {
		out.Dead = true
		return out
	}
	out.Escalated = true
	if err := r.tr.Kill(p.ID, true); err != nil {
		out.Err = err
		return out
	}
	out.Dead = ansaProbeGone(r.tr, p.ID)
	return out
}

// ansaNativeProgram is what the receipt calls a rysh agent. It is not a
// process name and must not read like one — the whole point of the native
// branch is that there was never a process here.
const ansaNativeProgram = "rysh agent (native turn)"

// killNativeTurn stops rysh's OWN agent in a pane that has no foreground
// program, and is the answer to the question Probe cannot answer.
//
// For a claude or a codex, "no foreground program" means idle: the agent IS
// the process, so no process means no agent. For a rysh agent it means nothing
// of the kind — the turn runs inside the daemon, in this pane's
// LLMPromptExecutionActor, and a pane driving a fifty-step tool loop looks
// identical to a bare shell from the outside.
//
// The stop is a CANCEL, not a signal, and that difference is a feature: the
// orchestrator's context is cancelled, the transcript is healed, and the run
// is checkpointed as paused — so `##ansa resume` continues the same turn
// rather than restoring a conversation and re-asking. There is no process to
// lose and therefore no in-flight response to lose either.
func (r *ansaRouter) killNativeTurn(out ansaKillOutcome, paneID string) ansaKillOutcome {
	inFlight, _, err := r.tr.TurnStatus(paneID)
	if err != nil {
		out.Err = err
		return out
	}
	if !inFlight {
		// Genuinely nothing running: a bare shell, or a rysh agent between
		// turns. Same verdict as before this branch existed, and now it is a
		// verdict rather than an assumption.
		out.Dead = true
		return out
	}
	out.Native = true
	out.WasRunning = ansaNativeProgram
	if err := r.tr.CancelTurn(paneID); err != nil {
		out.Err = err
		return out
	}
	out.Dead = ansaTurnStopped(r.tr, paneID)
	return out
}

// ansaTurnStopped polls the pane's agent until its turn is verifiably over.
//
// TWO CONSECUTIVE quiet samples, not one, and that is not belt-and-braces. A
// prompt delivered while a run was in flight is QUEUED by the executor and
// starts the moment the cancelled run's Done arrives — the pause is cleared
// and a fresh turn begins. That is F-41 in native clothing: a stop that a
// pending piece of work undoes a heartbeat later. One sample lands in the gap
// between the two runs and reports a stop that did not hold; two spanning
// samples do not.
//
// An error is not "stopped", for the same reason a failed probe is not "dead".
func ansaTurnStopped(tr ansaTransport, paneID string) bool {
	quiet := 0
	for i := 0; i < ansaKillVerifyRetries; i++ {
		time.Sleep(ansaKillVerifyPause)
		inFlight, _, err := tr.TurnStatus(paneID)
		if err != nil || inFlight {
			quiet = 0
			continue
		}
		quiet++
		if quiet >= 2 {
			return true
		}
	}
	return false
}

// ansaProbeGone polls Probe until the foreground program is gone or the
// budget runs out. A probe ERROR is not "gone": unverifiable must never be
// reported as dead — that is the receipt-without-delivery this track exists
// to kill, on its newest verb.
func ansaProbeGone(tr ansaTransport, paneID string) bool {
	for i := 0; i < ansaKillVerifyRetries; i++ {
		time.Sleep(ansaKillVerifyPause)
		prog, err := tr.Probe(paneID)
		if err == nil && prog == "" {
			return true
		}
	}
	return false
}

// KillFleet is the hard stop across a fleet ("" = every fleet), in the same
// §5.3a order as InterruptFleet — the board claude's own pane last, so the
// dispatch it is carrying cannot be cut off halfway.
func (r *ansaRouter) KillFleet(fleet string) ([]ansaKillOutcome, *msg.MsgAnsaRouteResult) {
	panes, err := r.tr.Panes()
	if err != nil {
		return nil, msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so no pane can be killed or ruled out: %v", err)
	}
	targets := ansaOrderFleetTargets(ansaFleetPanes(panes, fleet))
	if len(targets) == 0 {
		return nil, msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
			"no pane matches the fleet selector, so NOTHING was killed (%d panes seen)", len(panes))
	}
	out := make([]ansaKillOutcome, 0, len(targets))
	for _, p := range targets {
		out = append(out, r.killPane(p))
	}
	return out, nil
}

// ansaInterruptOutcome is what happened to one pane in a fleet-wide interrupt.
type ansaInterruptOutcome struct {
	// Drain is what the follow-up sweep achieved on this pane (F-30).
	Drain ansaDrainOutcome

	PaneID  string
	Persona string
	Role    string
	Err     error
}

// ansaFleetPanes selects the panes `--fleet` targets: every pane carrying a
// non-empty fleet.role, and nothing else.
//
// THERE IS NO EXCLUSION LIST, deliberately. The human's shells, the roadmap
// supervisor and the board claude's own pane are spared by CONSTRUCTION,
// because none of them carries fleet.* metadata. An exclusion list fails open —
// whatever nobody remembered to add gets interrupted — while selecting on
// presence fails closed: with fleet meta unpopulated this returns nothing, and
// `--fleet` interrupts NOTHING rather than everything.
//
// That asymmetry is the entire safety argument, so it is stated here rather
// than left to be inferred from the loop.
// ansaOrderFleetTargets puts the BOARD CLAUDE'S OWN PANE LAST.
//
// The board claude is what CARRIES a fleet-wide interrupt: a human types "stop
// all fleet" into the board, the board claude decides, and it runs this verb.
// If its own ESC lands before it has finished dispatching, it cancels the turn
// that is still doing the dispatching — the fleet is left half-stopped and the
// board reports a stop that never completed. That is receipt-without-delivery,
// self-inflicted, which is the one failure this whole track exists to kill.
// Founder ruling, 2026-08-10.
//
// ORDERED, NOT EXCLUDED — and that is the design decision, not an
// implementation detail. Excluding the board by name would be a second
// spare-list, which is precisely what the same ruling just reversed, and it
// would go stale the moment the pane is renamed or a second board exists.
// Ordering keeps ONE predicate (does the pane carry fleet.role) and satisfies
// the requirement by SEQUENCE instead of by exception: everyone else is
// dispatched first, and the board stops itself last — if it is a target at all.
// A board pane carrying no fleet meta is not a target and this is a no-op.
//
// Stable: every other pane keeps its relative order, so the only thing this
// changes is where the board sits.
func ansaOrderFleetTargets(targets []ansaPane) []ansaPane {
	out := make([]ansaPane, 0, len(targets))
	var board []ansaPane
	for _, p := range targets {
		if p.GivenName == boardAgentName {
			board = append(board, p)
			continue
		}
		out = append(out, p)
	}
	return append(out, board...)
}

// fleet == "" means EVERY fleet — the pre-028 behaviour, which is now reachable
// only by typing `--all-fleets`. A named fleet additionally requires
// fleet.name to match, so one fleet's stop cannot reach another's agents.
func ansaFleetPanes(panes []ansaPane, fleet string) []ansaPane {
	fleet = strings.TrimSpace(fleet)
	var out []ansaPane
	for _, p := range panes {
		if p.fleetRole() == "" {
			continue
		}
		if fleet != "" && !strings.EqualFold(p.fleetName(), fleet) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ansaFleetNames lists the fleets present in a session, in a stable order.
//
// It exists for the REFUSAL path and that is worth stating: when `--fleet
// epic-7` matches nothing, the useful answer is not "no such fleet" but "the
// session has epic-07 and board" — a stop that goes nowhere because of a typo
// is otherwise indistinguishable from a fleet that is already quiet.
func ansaFleetNames(panes []ansaPane) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range panes {
		if p.fleetRole() == "" {
			continue
		}
		name := p.fleetName()
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Interrupt cancels the turn in progress in one pane, addressed BY ID.
//
// NO LIVENESS PROBE, and this is the one place ANSA deliberately diverges from
// Route — so the reason is recorded rather than left as an inconsistency for
// someone to "fix". Route probes because a work order delivered to a dead pane
// is a lost work order. Interrupt is reached for when something is STUCK, and a
// stuck pane is precisely the pane least likely to answer a snapshot request:
// gating on a probe would refuse to interrupt exactly the panes that need it,
// and would report the refusal as prudence. ESC to a pane that has already
// exited is a no-op, so the cautious version costs more than it protects.
//
// What this therefore does NOT claim, stated because this track punishes
// implied limits: it reports that ESC was PUBLISHED, not that a turn ended.
// Nothing acknowledges a raw key.
func (r *ansaRouter) Interrupt(paneID string) *msg.MsgAnsaRouteResult {
	to := strings.TrimSpace(paneID)
	if to == "" {
		return msg.AnsaRefusal(msg.AnsaErrNoTarget, "no target: name a pane id")
	}

	panes, err := r.tr.Panes()
	if err != nil {
		return msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so %q can be neither found nor ruled out: %v", to, err)
	}

	target, found := ansaPaneByID(panes, to)
	if !found {
		// Same split as Route: a name arriving here is a caller that skipped
		// edge resolution, and it must not be resolved silently.
		if ansaNameExists(panes, to) {
			return msg.AnsaRefusal(msg.AnsaErrNotAnID,
				"%q is a pane NAME, not a pane id — ansa addresses by pane uuid on the wire; "+
					"resolve @name at the edge and send the id", to)
		}
		return msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
			"no pane with id %q in this session", to)
	}

	if err := r.tr.Interrupt(target.ID); err != nil {
		return msg.AnsaRefusal(msg.AnsaErrPublishFailed,
			"interrupt for pane %s failed, nothing was sent: %v", target.ID, err)
	}
	return &msg.MsgAnsaRouteResult{
		OK:            true,
		TargetPaneID:  target.ID,
		TargetPersona: ansaPersona(target),
	}
}

// InterruptFleet interrupts every pane carrying a fleet.role.
//
// An empty selection is a REFUSAL, not a quiet success. "0 panes interrupted"
// printed as a normal result reads as "the fleet is idle"; it far more often
// means fleet metadata was never written, and the operator needs to know that
// the command did nothing rather than that there was nothing to do.
// fleet names which fleet to stop; "" is every fleet (`--all-fleets`).
func (r *ansaRouter) InterruptFleet(fleet string) ([]ansaInterruptOutcome, *msg.MsgAnsaRouteResult) {
	panes, err := r.tr.Panes()
	if err != nil {
		return nil, msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so no pane can be interrupted or ruled out: %v", err)
	}

	targets := ansaOrderFleetTargets(ansaFleetPanes(panes, fleet))
	if len(targets) == 0 {
		// TWO DIFFERENT FACTS, AND THEY MUST NOT BE COLLAPSED. "this session
		// runs no fleet at all" and "you named a fleet that is not here" lead
		// to different next actions, and the second is usually a typo — which,
		// reported as the first, reads as "the fleet is already stopped".
		if fleet != "" {
			if names := ansaFleetNames(panes); len(names) > 0 {
				return nil, msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
					"no pane carries %s=%q, so NOTHING was interrupted (%d panes seen). "+
						"This session runs: %s", ansaMetaFleetName, fleet, len(panes),
					strings.Join(names, ", "))
			}
			return nil, msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
				"no pane carries %s=%q, and no pane in this session carries %s at all, "+
					"so NOTHING was interrupted (%d panes seen)",
				ansaMetaFleetName, fleet, ansaMetaFleetName, len(panes))
		}
		return nil, msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
			"no pane in this session carries %s metadata, so NOTHING was interrupted "+
				"(%d panes seen). --all-fleets selects on that key alone; a pane without it is "+
				"spared by construction", ansaMetaFleetRole, len(panes))
	}

	// One pane's failure must not stop the rest. A partial interrupt reported
	// per pane is recoverable; an abort halfway through, reported as one error,
	// leaves the operator unable to tell which half ran.
	out := make([]ansaInterruptOutcome, 0, len(targets))
	for _, p := range targets {
		out = append(out, ansaInterruptOutcome{
			PaneID:  p.ID,
			Persona: ansaPersona(p),
			Role:    p.fleetRole(),
			Err:     r.tr.Interrupt(p.ID),
		})
	}

	// PHASE 2 — drain. The ordered sweep above stops the turn each pane is in;
	// its queue then starts the next order on its own, which is why "stop all
	// fleet" used to leave the fleet working seconds later. Founder decision
	// 2026-08-10: clear the queue, accepting that queued orders are cancelled.
	//
	// Deliberately a SECOND PASS rather than a drain inside the loop above: the
	// first sweep must reach every pane fast and in the §5.3a order (the board
	// claude last, so its own ESC cannot cut off the dispatch). Draining pane 1
	// for five seconds before pane 2 is even asked to stop would destroy that.
	for i := range out {
		if out[i].Err != nil {
			continue // never chase a pane the first interrupt could not reach
		}
		out[i].Drain = ansaDrainPane(r.tr, out[i].PaneID)
	}

	// PHASE 3 — re-sweep, because a stopped pane can WAKE ITSELF and the drain
	// cannot see it coming. A backgrounded Bash task keeps running across
	// turns and re-invokes the session when it exits: the task-notification
	// arrives as a new user turn, no keystroke involved. Fleet agents
	// habitually run background collects, so a stopped fleet is a fleet full
	// of scheduled wake-up calls; an in-flight sibling delivery lands the same
	// way. Scan the whole target set a few more times and put back down
	// whatever woke. The first scan is a baseline; detection needs the marker
	// OR a changed screen, because at fleet pane widths the marker is not
	// rendered at all (F-32).
	last := make(map[string]string, len(out))
	for round := 0; round < ansaFleetResweeps; round++ {
		time.Sleep(ansaResweepSettle)
		for i := range out {
			if out[i].Err != nil || out[i].Drain.Unreadable != nil {
				continue
			}
			rows, err := r.tr.Screen(out[i].PaneID)
			if err != nil {
				continue // transient; the next round looks again
			}
			cur := strings.Join(rows, "\n")
			prev, seen := last[out[i].PaneID]
			last[out[i].PaneID] = cur
			woke := ansaScreenIsWorking(rows) || (seen && cur != prev)
			if !woke {
				continue
			}
			if err := r.tr.Interrupt(out[i].PaneID); err != nil {
				continue
			}
			out[i].Drain.Rewoke++
			d := ansaDrainPane(r.tr, out[i].PaneID)
			out[i].Drain.Rounds += d.Rounds
			out[i].Drain.Quiet = d.Quiet
			if d.Unreadable != nil {
				out[i].Drain.Unreadable = d.Unreadable
			}
			// Re-baseline after the put-down, or the settle-back redraw
			// reads as another wake next round.
			if rows2, err2 := r.tr.Screen(out[i].PaneID); err2 == nil {
				last[out[i].PaneID] = strings.Join(rows2, "\n")
			}
		}
	}
	return out, nil
}

// ansaPanesFromWorkspace flattens a workspace snapshot into addressable panes.
func ansaPanesFromWorkspace(snap *domain.WorkspaceSnapshot) []ansaPane {
	var out []ansaPane
	for i := range snap.Tabs {
		tab := &snap.Tabs[i]
		for p := range domain.PanesInTab(tab) {
			out = append(out, ansaPane{
				ID:        p.ID,
				GivenName: p.GivenName,
				Title:     p.Title,
				Meta:      ansaCopyMeta(p.Meta),
			})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// the actor
// ---------------------------------------------------------------------------

// AgentNervousSystemActor is the session-scoped router. One per session, always
// on, spawned by the WorkspaceActor.
//
// PER SESSION, NOT PER FLEET — the founder's ruling. Fleet is metadata on a
// message, never a boundary on an actor, so this actor knows nothing about
// fleets and a non-fleet claude routes exactly like a fleet member.
type AgentNervousSystemActor struct {
	pub *msg.NATSPublisher
	nc  *nats.Conn

	br     *bridge.NATSBridge
	router *ansaRouter
}

// NewAgentNervousSystemActor builds the router over the live session.
func NewAgentNervousSystemActor(pub *msg.NATSPublisher, nc *nats.Conn) *AgentNervousSystemActor {
	return &AgentNervousSystemActor{
		pub:    pub,
		nc:     nc,
		router: &ansaRouter{tr: &natsAnsaTransport{pub: pub}},
	}
}

// Receive implements actor.Actor.
func (a *AgentNervousSystemActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		a.br = bridge.New(a.nc, ctx.Self(), ctx.ActorSystem(), a.pub.Codecs())
		if err := a.br.AddSubject(msg.AnsaInboxSubject()); err != nil {
			// Loud, not swallowed: an ANSA that failed to subscribe would
			// answer nothing, and every route through it would time out with
			// no explanation anywhere.
			slog.Error("ansa: subscribe inbox failed — routing is DOWN for this session",
				"subject", msg.AnsaInboxSubject(), "err", err)
			return
		}
		slog.Info("ansa: routing", "subject", msg.AnsaInboxSubject())

	case *actor.Stopping:
		if a.br != nil {
			a.br.Stop()
			a.br = nil
		}

	case *msg.RequestEnvelope:
		// The only shape ANSA answers. A route is a REQUEST: the reply is the
		// point, and a fire-and-forget route would reintroduce the silent drop.
		route, ok := m.Inner.(*msg.MsgAnsaRoute)
		if !ok {
			_ = m.Reply(msg.AnsaRefusal(msg.AnsaErrNoTarget,
				"ansa answers %T only, got %T", &msg.MsgAnsaRoute{}, m.Inner))
			return
		}
		_ = m.Reply(a.router.Route(route))

	case *msg.MsgAnsaRoute:
		// Fire-and-forget arrival. Refused deliberately and logged rather than
		// routed: with no reply subject there is no way to tell the sender what
		// happened, which is precisely the failure mode this actor exists to
		// remove. Answering "no" loudly beats delivering blind.
		slog.Warn("ansa: route dropped — sent without a reply subject; use a request, not a publish",
			"to", m.To, "from", m.From)
	}
}
