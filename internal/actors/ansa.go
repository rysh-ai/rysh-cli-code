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
	// Probe reports whether a pane is alive and pumping its mailbox.
	Probe(paneID string) error
	// Deliver publishes the correctly typed message to the pane's inbox.
	Deliver(paneID, mode, text string) error
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
	if err := r.tr.Probe(target.ID); err != nil {
		return msg.AnsaRefusal(msg.AnsaErrUnreachable,
			"pane %s (%s) did not answer — it may have exited; nothing was delivered: %v",
			target.ID, ansaPersona(target), err)
	}

	if err := r.tr.Deliver(target.ID, mode, text); err != nil {
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

func (t *natsAnsaTransport) Probe(paneID string) error {
	_, err := t.pub.Request(
		msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true},
		ansaProbeTimeout,
	)
	return err
}

func (t *natsAnsaTransport) Deliver(paneID, mode, text string) error {
	return ansaDeliverToInbox(t.pub, paneID, mode, text)
}

// ansaDeliverToInbox publishes the message type the PaneActor actually handles.
//
// This is not a detail. The PaneActor's Receive has cases for MsgPaneExecShell
// and MsgPaneExecPrompt; MsgSubmitInput is handled by the PaneGroupActor in the
// normal routing chain and has NO case here, so sending one to a pane inbox is
// published successfully and then dropped on the floor — a silent loss with a
// nil error, which is the exact thing ANSA exists to prevent. Both
// internal/tools/pane_send.go and internal/cli/commands.go carry the same
// warning in their own words; this is the third site that has had to learn it.
func ansaDeliverToInbox(pub *msg.NATSPublisher, paneID, mode, text string) error {
	subject := msg.T("pane", paneID, "inbox")
	if mode == msg.AnsaModePrompt {
		return pub.Send(subject, &msg.MsgPaneExecPrompt{Prompt: text})
	}
	return pub.Send(subject, &msg.MsgPaneExecShell{Command: text})
}

// ansaPanesFromWorkspace flattens a workspace snapshot into addressable panes.
func ansaPanesFromWorkspace(snap *domain.WorkspaceSnapshot) []ansaPane {
	var out []ansaPane
	for i := range snap.Tabs {
		tab := &snap.Tabs[i]
		for p := range domain.PanesInTab(tab) {
			out = append(out, ansaPane{ID: p.ID, GivenName: p.GivenName, Title: p.Title})
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
