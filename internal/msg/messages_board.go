// SPDX-License-Identifier: Apache-2.0

package msg

import "strconv"

// agents-board (design 025) — the message contract for a threaded, push-based
// view of what every agent in a session is doing.
//
// Two things about this file are load-bearing and were paid for by wave-1
// verification against the running code; do not "simplify" either away.
//
//  1. PANE ID IS THE IDENTITY, PERSONA IS ONLY A LABEL. A pane's given-name is
//     unique per LANE, not per session (TabActor.IsGivenNameTakenInLane,
//     internal/actors/tab_snapshot.go), and fleet managers and workers live in
//     different lanes — so two panes may legally carry the same given-name.
//     Anything that keys off Persona will merge two agents into one.
//
//  2. THREAD IDS ARE MINTED BY THE POSTER, not handed back by the board. The
//     round trip does work on the CLI path, but the humanoid tool path reads
//     output through a quiet-period collector that can legitimately return
//     "(no output within 5s)" (internal/tools/rysh_command.go) — an id an agent
//     SOMETIMES gets is worse than one it never needs. Agent-minted ids are also
//     idempotent under retry, which a round trip is not. The cost is that a
//     reply can arrive before its root; the board re-parents orphans instead.

// BoardSchemaVersion is stamped on every board message from the first release.
// A board that cannot tell v1 from v2 cannot be changed later without breaking
// every live agent, so this ships before there is anything to be compatible
// with.
const BoardSchemaVersion = 1

// Board message kinds. Kind is deliberately a free-form string rather than an
// enum: fleetctl's --kind is already free-form (it carries WORK ORDER, REPORT,
// PROGRESS, BLOCKED, NUDGE and whatever an agent invents next), and the board
// must render an unknown kind rather than drop the message. These are the
// well-known values, not the permitted set.
const (
	BoardKindMilestone = "milestone"
	BoardKindTaskDone  = "task-done"
	BoardKindBlocked   = "blocked"
	BoardKindReply     = "reply"
)

// MsgBoardPost is one message on the agents board: a root, or a reply under a
// root. Published on T("board", "post"); the board view subscribes.
type MsgBoardPost struct {
	V int `json:"v"` // BoardSchemaVersion

	// PaneID is the FULL pane uuid of the poster and is the identity key.
	// Never the 8-char truncation the fleet envelope carries.
	PaneID string `json:"pane_id"`

	// Persona is the poster's display name — its pane given-name, falling back
	// to the auto-title and then to "pane-<first 8 of uuid>". Display only; not
	// unique (see the file header).
	Persona string `json:"persona"`

	Kind string `json:"kind"` // free-form; see BoardKind* for the well-known ones
	Text string `json:"text"`

	// ThreadID is empty when this post IS a root. Otherwise it is the root's
	// id, minted by whichever agent opened the thread as "<pane-uuid>/<n>".
	// A reply whose root has not arrived yet is rendered as a provisional root
	// and re-parented when the root lands — that is expected, not an error.
	ThreadID string `json:"thread_id,omitempty"`

	TS int64 `json:"ts"` // unix millis, the POSTER's clock: arrival order, not causal order

	// ToPersona / ToPaneID — the recipient of a DIRECTED message.
	//
	// GATE 4 WAS REOPENED (founder, 2026-08-09) and this reverses the earlier
	// ruling that removed these. The original argument for asking before v1 was
	// that adding them later is a breaking change; the schema is still V=1, so
	// it is still cheap.
	//
	// A directed message is still CHAT, not routing: this says who was spoken
	// TO, exactly as a chat app shows an @mention. It carries no chain, no
	// msg id and no delivery semantics, and ANSA — not this field — is what
	// actually delivers anything.
	//
	// Both are empty for a broadcast, which stays the default. Gate 3 is
	// unaffected: a non-fleet claude directs a message exactly as a fleet one
	// does, because neither field mentions a fleet.
	ToPersona string `json:"to_persona,omitempty"`
	ToPaneID  string `json:"to_pane_id,omitempty"`

	// STILL DELIBERATELY ABSENT: Envelope, Fleet, Role, Unit.
	//
	// The "FROM manager … TO worker …" routing envelope remains a fleet
	// concern and does not enter this schema — gate 4's reopening added a
	// recipient, not the chain. With no fleet fields, a non-fleet claude posts
	// through exactly the same shape as a fleet one, and nothing downstream can
	// start treating fleet posts as first-class. Do not add them back without a
	// founder ruling.
}

// MsgBoardRegister is a persona announcement: an agent telling the board who it
// is, so the board can show a roster without waiting for a first post.
// Published on T("board", "register"). Registration is advisory — a post from an
// unregistered pane is still rendered, because a board that drops messages from
// agents that forgot to introduce themselves is worse than one with a thin
// roster.
type MsgBoardRegister struct {
	V       int    `json:"v"`
	PaneID  string `json:"pane_id"`
	Persona string `json:"persona"`
	TS      int64  `json:"ts"`
}

// MsgCLIBoardPost is the agent-facing door: `rysh --board-post --as <pane-id>`.
//
// It exists as its OWN CLI message rather than riding MsgCLIRyshCommand
// because that path has three properties a board post must not have, each
// verified against the running code in wave 1:
//
//   - with no --pane-id it runs as the workspace's ACTIVE pane, so a post would
//     be attributed to a bystander (WorkspaceActor.handleCLIRyshCommand);
//   - naming a pane calls focusPaneByID, which switches the active tab and moves
//     the human's focus — unusable when dozens of agents post;
//   - runRyshCommand echoes the command line into the pane's output buffers, so
//     a post would pollute whichever pane it was attributed to.
//
// So this message carries its poster EXPLICITLY in AsPaneID, never inherits the
// ambient pane, and its handler neither focuses nor echoes.
type MsgCLIBoardPost struct {
	// AsPaneID is the poster, and is REQUIRED. An empty value is an error, not
	// a fallback to the active pane — silently attributing a post to a
	// bystander is the exact defect this message exists to avoid.
	AsPaneID string `json:"as_pane_id"`

	Kind     string `json:"kind,omitempty"` // default: BoardKindMilestone
	Text     string `json:"text"`
	ThreadID string `json:"thread_id,omitempty"` // empty = start a new root

	// Board names which board to post to (design 028). Empty means "resolve it
	// from the poster", which the workspace does: the pane's own board.id meta,
	// then the session board.
	//
	// It rides the CLI message and NOT MsgBoardPost, and the difference is the
	// whole of D-12: this is a request that says where to deliver, while the
	// post itself stays free of any board or fleet identity. Adding it here
	// costs nothing to gate 4, which is about the schema on the wire between
	// agents.
	Board string `json:"board,omitempty"`
}

// NewBoardPost stamps the schema version and the poster's clock. Every producer
// goes through here so that V is never forgotten on a new call site.
func NewBoardPost(paneID, persona, kind, text string, nowMillis int64) *MsgBoardPost {
	if kind == "" {
		kind = BoardKindMilestone
	}
	return &MsgBoardPost{
		V:       BoardSchemaVersion,
		PaneID:  paneID,
		Persona: persona,
		Kind:    kind,
		Text:    text,
		TS:      nowMillis,
	}
}

// NewBoardRegister stamps the schema version and the announcer's clock, for the
// same reason NewBoardPost does: every producer goes through a constructor so V
// is never forgotten on a new call site.
func NewBoardRegister(paneID, persona string, nowMillis int64) *MsgBoardRegister {
	return &MsgBoardRegister{
		V:       BoardSchemaVersion,
		PaneID:  paneID,
		Persona: persona,
		TS:      nowMillis,
	}
}

// MintThreadID builds a thread id the poster owns outright: "<pane-uuid>/<n>".
//
// It is derived from the FULL pane uuid, not the persona and not the envelope's
// 8-char truncation, so two agents can never mint colliding ids even when they
// share a given-name across lanes. No round trip to the board is involved,
// which is what lets an agent open a thread while blind to the board — and what
// makes a retried post idempotent instead of minting a second root.
func MintThreadID(paneID string, n int) string {
	return paneID + "/" + strconv.Itoa(n)
}

// BoardPersona resolves a display name for a pane, in the order the board
// renders it: given-name, then the auto-generated title, then "pane-<8>".
//
// It never returns an empty string and never returns the literal "no-name" that
// ##pane info prints for an unnamed pane — an unnamed agent still needs a
// stable label to be followed by.
//
// The approval-pane guard is not hypothetical: approval panes OVERLOAD
// GivenName to carry "requestID\x1FresponseSubject" for the TUI
// (internal/actors/approval_pane.go). Rendering that blindly would one day
// print a NATS subject as somebody's name, so a name carrying the unit
// separator is rejected and falls through to the next candidate.
func BoardPersona(givenName, title, paneID string) string {
	if givenName != "" && !containsUnitSep(givenName) {
		return givenName
	}
	if title != "" {
		return title
	}
	if len(paneID) >= 8 {
		return "pane-" + paneID[:8]
	}
	if paneID != "" {
		return "pane-" + paneID
	}
	return "pane-unknown"
}

func containsUnitSep(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1f {
			return true
		}
	}
	return false
}

// BoardAliveSubject is where the recorder answers "are you recording?".
//
// A REQUEST, not a heartbeat. Every failure on this track came from inferring
// state from a proxy — a pane's idle flag, send.ok, os.path.isfile, a persisted
// roster entry, a KV revision. A heartbeat is one more proxy: it reports that
// something wrote a key recently, which is not the same as a recorder being
// alive now, and it needs a staleness threshold that can be wrong under load.
//
// Asking the recorder directly has no threshold to tune, and a timeout tests
// the ACTUAL path rather than an artifact's freshness. It also keeps liveness
// out of the board's KV bucket, where a second writer to that bucket would
// falsify the single-writer detector in internal/actors/abla_test.go — that
// bucket's precondition is "nothing else writes here", and liveness has no
// business breaking it.
// Per board since design 028: a caller asks whether the recorder for a NAMED
// board is listening. One recorder serves every board in a session today
// (internal/actors/abla.go), so every board's alive subject is answered by the
// same actor — but the subject is per board so that stays an implementation
// detail rather than something callers encode.
func BoardAliveSubject(board string) string { return boardSubject(board, "alive") }

// BoardAlivePattern is the wildcard a recorder subscribes to in order to answer
// for every named board. Like the post pattern, it does not match the default
// board's shorter subject.
func BoardAlivePattern() string { return T("board", "*", "alive") }

// BoardAliveReply is what a live recorder answers. The content is deliberately
// trivial: the fact that a reply arrived at all is the signal.
const BoardAliveReply = "recording"

// BoardQuerySubject is where the recorder answers "what is ON the board?".
//
// A SECOND REQUEST SUBJECT IN THE SHAPE OF BoardAliveSubject, deliberately, and
// served by the same actor for the same two reasons.
//
//  1. ASK THE RECORDER, DO NOT READ THE TRACE IT LEFT (design 026 §5.4). ABLA
//     holds the authoritative in-memory board — restored from the KV at start
//     and fed by the live subscription since. Its answer is what the session
//     actually heard. The KV is a durable copy of that, one restore behind, and
//     reading it would be inferring the board's state from an artifact instead
//     of asking the thing that owns it.
//
//  2. F-23 IS WHAT HAPPENS WHEN A SECOND CALL SITE DERIVES THE BUCKET NAME.
//     runAttachUI built a bus.Config with no SessionName, so an attaching TUI
//     opened rysh-board-default while the daemon wrote rysh-board-<session>.
//     Subjects were fine; only the restore was dead, and it failed LOOKING
//     HEALTHY — an empty board renders identically to a quiet one. A read path
//     that goes through this subject cannot reintroduce that: it has no bucket
//     name to get wrong, and a recorder that does not answer is an ERROR rather
//     than an empty result (board.ErrNoRecorder).
func BoardQuerySubject(board string) string { return boardSubject(board, "query") }

// BoardQueryPattern is the wildcard the recorder serves every named board on.
func BoardQueryPattern() string { return T("board", "*", "query") }
