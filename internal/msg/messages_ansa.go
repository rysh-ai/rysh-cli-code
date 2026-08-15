// SPDX-License-Identifier: Apache-2.0

package msg

import "fmt"

// ANSA — the Agent Nervous System Actor. One per SESSION, always on, a router:
// an agent names a target, ANSA delivers to that pane's inbox.
//
// Founder ruling: ANSA is per SESSION, not per fleet. Fleet is metadata on a
// message, never a boundary on an actor — so nothing in this file mentions a
// fleet, and a non-fleet claude routes exactly like a fleet member.
//
// ANSA IS NOT NEW TRANSPORT. The per-pane inbox (T("pane", id, "inbox"))
// already exists and already works; ANSA is a mediator in front of it that adds
// the one thing a raw publish cannot give a caller: an ANSWER. A publish to a
// NATS subject succeeds whether or not anything is listening, whether or not
// the target exists, and whether or not the payload is a type the recipient
// understands. That is the failure this contract exists to remove.
//
// THE RULE THAT SHAPES EVERYTHING HERE, from the CEO, verbatim:
//
//	a board post is worth less than the control channel;
//	a work order is worth more than either.
//
// So ANSA must fall back or fail loudly — NEVER silently drop. Every refusal
// carries a machine-readable AnsaErr* code as well as prose, because a caller
// that has to tell "the target is gone" from "the name matched two panes" in
// order to react cannot be asked to parse a sentence.

// AnsaSchemaVersion is stamped on every route request, from the first release,
// for the same reason the board's is: a router that cannot tell v1 from v2
// cannot be changed later without breaking every live agent.
const AnsaSchemaVersion = 1

// Delivery modes. These are the pane inbox's own vocabulary, not ANSA's
// invention — see MsgPaneExecShell / MsgPaneExecPrompt.
const (
	AnsaModeShell  = "shell"
	AnsaModePrompt = "prompt"
)

// Refusal codes. Every one of these is a message that did NOT get delivered and
// a caller that WAS told so.
//
// They are exhaustive by construction: the router's only success path returns
// OK, and every other branch sets one of these. A new failure mode must add a
// code here, which is a visible edit rather than a silent `return nil`.
const (
	// AnsaErrNoTarget — the caller named nobody.
	AnsaErrNoTarget = "no_target"
	// AnsaErrNoText — the caller sent nothing to deliver.
	AnsaErrNoText = "no_text"
	// AnsaErrUnknownTarget — the name/id matched no pane in the session.
	AnsaErrUnknownTarget = "unknown_target"
	// AnsaErrAmbiguousTarget — the name matched MORE THAN ONE pane. ANSA
	// refuses rather than picking one. See the note on given-names below.
	AnsaErrAmbiguousTarget = "ambiguous_target"
	// AnsaErrUnreachable — the target pane exists in the layout but did not
	// answer a liveness probe, so publishing to its inbox would be a write into
	// a subject nobody is reading.
	AnsaErrUnreachable = "unreachable"
	// AnsaErrBadMode — an unrecognised delivery mode. NOT defaulted silently:
	// a typo'd mode that quietly became a shell command is a message delivered
	// as something other than what the sender meant.
	AnsaErrBadMode = "bad_mode"
	// AnsaErrPublishFailed — the transport itself refused.
	AnsaErrPublishFailed = "publish_failed"
	// AnsaErrNotAnID — the router was handed a NAME where an id belongs. The
	// caller skipped edge resolution. Its own code because it is a caller bug
	// with a specific fix ("resolve the @name first"), not a missing pane.
	AnsaErrNotAnID = "not_an_id"
	// AnsaErrDirectory — ANSA could not enumerate the session's panes, so it
	// cannot know whether the target exists. Distinct from unknown_target on
	// purpose: "I looked and it is not there" and "I could not look" are
	// different facts, and only one of them means the caller should give up.
	AnsaErrDirectory = "directory_unavailable"
)

// MsgAnsaRoute is a routing request: deliver Text to the pane named by To.
//
// Published to T("ansa", "inbox") as a REQUEST — the reply is
// MsgAnsaRouteResult and is the whole point. Fire-and-forget would reintroduce
// exactly the silent drop this actor exists to prevent.
type MsgAnsaRoute struct {
	V int `json:"v"` // AnsaSchemaVersion

	// From is the sender's pane id, for attribution and for the audit line the
	// target sees. Optional: a route from something that is not a pane (cron, a
	// humanoid, a test) is legitimate and must not be refused for it.
	From string `json:"from,omitempty"`

	// To is the target's FULL PANE UUID. Always. A name never travels as an
	// address.
	//
	// @name is resolved to an id at the EDGE — in the command that accepted the
	// @ — never in the router. The reason is one layer below the board's:
	// given-names are unique per LANE, not per session
	// (TabActor.IsGivenNameTakenInLane), so a name is a label for humans and an
	// id is an address. A name arriving here is AnsaErrNotAnID, because
	// resolving it late would re-open the ambiguity the edge already closed.
	//
	// Ambiguity is therefore HANDLED rather than assumed away, which is why
	// session-unique given-names are an improvement to this design and not a
	// safety gate on it.
	To string `json:"to"`

	// Mode is AnsaModeShell or AnsaModePrompt. Empty means shell, which is the
	// pane inbox's own default; anything else is AnsaErrBadMode.
	Mode string `json:"mode,omitempty"`

	Text string `json:"text"`
}

// MsgAnsaRouteResult is the answer. OK is the only success; every false carries
// both a Code (to branch on) and an Error (to show a human).
type MsgAnsaRouteResult struct {
	OK bool `json:"ok"`

	// TargetPaneID is the FULL pane uuid the message was delivered to —
	// resolved, never echoed back from the request. A caller that addressed by
	// name learns which pane it actually reached.
	TargetPaneID string `json:"target_pane_id,omitempty"`
	// TargetPersona is that pane's display name, for the caller's own logs.
	TargetPersona string `json:"target_persona,omitempty"`

	// Code is one of the AnsaErr* constants when OK is false, and empty when
	// OK is true.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	// Candidates lists the panes an ambiguous name matched, so the caller can
	// re-address by id without having to go and look. Only set for
	// AnsaErrAmbiguousTarget — a refusal that tells you how to succeed.
	Candidates []string `json:"candidates,omitempty"`
}

// MsgCLIAnsaSend is the AGENT door, reached by `rysh ansa send`.
//
// It is its own CLI message rather than a ##ansa run through
// MsgCLIRyshCommand, for the three reasons the board's agent door exists (see
// internal/actors/workspace_board.go): that path attributes to the workspace's
// ambient active pane when none is named, FOCUSES the pane that is named, and
// ECHOES the command line into that pane's output buffers. A control channel
// used by dozens of agents must do none of the three — an agent routing a
// message must not yank the human's cursor to its own pane.
type MsgCLIAnsaSend struct {
	// AsPaneID is the SENDER, and unlike the board's it is optional: routing
	// from outside a pane is legitimate. It is never inferred from the active
	// pane, because a wrong "from" is a lie about who is talking.
	AsPaneID string `json:"as_pane_id,omitempty"`

	// To is required — the whole operation is "deliver to this target".
	To   string `json:"to"`
	Mode string `json:"mode,omitempty"`
	Text string `json:"text"`
}

// AnsaInboxSubject is where the router listens. Session-scoped like every other
// subject: msg.T renders the session prefix, so this is never a literal.
func AnsaInboxSubject() string { return T("ansa", "inbox") }

// NewAnsaRoute stamps the schema version so V is never forgotten at a new call
// site — the same reason NewBoardPost exists.
func NewAnsaRoute(from, to, mode, text string) *MsgAnsaRoute {
	return &MsgAnsaRoute{V: AnsaSchemaVersion, From: from, To: to, Mode: mode, Text: text}
}

// AnsaRefusal builds a failed result. Constructing refusals through one
// function is what keeps "a code was set" from being a thing anyone has to
// remember at eight separate return statements.
func AnsaRefusal(code, format string, a ...any) *MsgAnsaRouteResult {
	return &MsgAnsaRouteResult{OK: false, Code: code, Error: fmt.Sprintf(format, a...)}
}

// MsgBoardAgentPrompt is one line typed into the agents-board input field
// (design 027 §5.2).
//
// It carries NO target. Every prompt goes to the board claude — that is the
// founder's ruling 2, and there is deliberately no verbatim `@tag` bypass — so
// naming a target here would create a second way to route that the board claude
// is not in the path of. Resolving which pane is the board claude belongs to the
// workspace, which is also where the refusal lives when two panes share the
// name.
type MsgBoardAgentPrompt struct {
	Text string `json:"text"`

	// Board names WHICH board's claude to reach (design 028, `D-13` ruled
	// per-fleet on 2026-08-11). Empty means the session board.
	//
	// It does not weaken ruling 2: every prompt still goes to a board claude
	// and there is still no bypass. What it fixes is that with one mind per
	// fleet, "the board claude" stopped being a single pane — and a prompt
	// typed into fleet epic-07's board must not be acted on by epic-08's.
	Board string `json:"board,omitempty"`
}
