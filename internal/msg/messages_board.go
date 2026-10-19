// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

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

	// BoardKindCmd and BoardKindAck are the two kinds that are NOT chat
	// (design 050, polling-stream). A cmd is a work order addressed to ONE
	// pane (ToPaneID), carrying a CmdID; the courier delivers it into that
	// agent's own inbox beside its keyboard, never through its PTY. An ack is
	// the recipient saying "got it", filed on the command's own thread and
	// naming the same CmdID. Every other kind is status, and nothing consumes
	// status as work.
	BoardKindCmd = "cmd"
	BoardKindAck = "ack"
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
	// On every kind EXCEPT BoardKindCmd these are still chat: who was spoken
	// TO, as a chat app shows an @mention, with no delivery behind it.
	//
	// ON A cmd THEY ARE THE ADDRESS (design 050, founder 2026-10-11: the board
	// becomes the fleet's message bus). ToPaneID is required, is always the
	// FULL pane uuid (the door resolves a name to it), and the courier delivers
	// the command to exactly that pane. ToPersona stays display-only, for the
	// file header's reason: a given-name is not unique per session.
	//
	// Both are empty for a broadcast, which stays the default. Gate 3 is
	// unaffected: a non-fleet claude directs a message exactly as a fleet one
	// does, because neither field mentions a fleet.
	ToPersona string `json:"to_persona,omitempty"`
	ToPaneID  string `json:"to_pane_id,omitempty"`

	// CmdID names a command (BoardKindCmd) and, on an ack (BoardKindAck), the
	// command being acknowledged. Empty on every other kind.
	//
	// Minted by the SENDER, like ThreadID and for the same two reasons: no
	// round trip, and a retried send is idempotent rather than a second order.
	// A caller may supply its own (fleetctl passes its msg-NNNN); otherwise the
	// door mints one with MintCmdID. Unique per sender, not per session — the
	// pair (PaneID, CmdID) is the identity, which is why a command's thread is
	// CmdThreadID(PaneID, CmdID).
	CmdID string `json:"cmd_id,omitempty"`

	// File is an absolute path the recipient must read in full, for an order
	// too long to carry inline (fleetctl's --file). Optional; cmd only.
	File string `json:"file,omitempty"`

	// STILL DELIBERATELY ABSENT: Envelope, Fleet, Role, Unit. (CmdID and File
	// are not fleet fields: a non-fleet claude sends a command exactly as a
	// fleet one does.)
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

	// To is the recipient of a command (Kind == BoardKindCmd): a pane id, an
	// auto-title or a given-name, resolved by the door to a full pane uuid.
	// Required on a cmd and REFUSED on every other kind — a directed status
	// post that nothing delivers would read like an order that was sent.
	To string `json:"to,omitempty"`

	// CmdID is optional on a cmd (the door mints one) and required on an ack.
	CmdID string `json:"cmd_id,omitempty"`

	// File is an absolute path for the recipient to read in full; cmd only.
	// The CLI checks it exists before sending.
	File string `json:"file,omitempty"`
}

// MsgCLIBoardResolve asks the workspace who a pane is and which board it is on:
// `rysh board inbox` and `rysh board ack` need both before they can read.
//
// Its own message for the reason MsgCLIBoardPost is: the ## path would focus
// the pane and echo into it. The reply is a MsgCLIResponse with ID = the full
// pane uuid and Output = the board id.
type MsgCLIBoardResolve struct {
	AsPaneID string `json:"as_pane_id"`
	// WantCaps asks the daemon to append its board capabilities to Output as
	// a second line, "caps: <a> <b> ...". Opt-in so the first line stays the
	// bare board id for every caller; a daemon that predates the field ignores
	// it and answers with no caps line, which IS the answer "no capabilities".
	WantCaps bool `json:"want_caps,omitempty"`
}

// BoardCaps are the board capabilities this daemon reports (design 051):
// "queue" means its courier leaves queue-messaging panes to their own
// listeners. A daemon without it would deliver their prompts twice.
var BoardCaps = []string{"queue"}

// cmdIDPattern bounds what a caller-supplied CmdID may contain. It ends up in a
// thread id, a KV key (design 050 §5.2) and an envelope an agent reads, so it is
// kept to characters that are safe in all three.
var cmdIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateCmdID refuses a CmdID that could not travel safely.
func ValidateCmdID(id string) error {
	if !cmdIDPattern.MatchString(id) {
		return fmt.Errorf("invalid command id %q: want 1-64 of [A-Za-z0-9._-], starting with a letter or digit", id)
	}
	return nil
}

// MintCmdID builds a command id the sender owns: "cmd-<base36 millis>-<4 hex>".
// The random suffix covers two sends in the same millisecond from one pane.
func MintCmdID(nowMillis int64) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return "cmd-" + strconv.FormatInt(nowMillis, 36) + "-" + hex.EncodeToString(b[:])
}

// CmdThreadID is the thread a command opens: "<sender-pane-uuid>/<cmd-id>".
//
// It has the shape of a MintThreadID id on purpose, so the board's store files
// the command as the ROOT of its own thread (it owns the prefix) and every reply
// and ack lands under it. The recipient's ack and `rysh board reply` both
// address this id.
func CmdThreadID(senderPaneID, cmdID string) string {
	return senderPaneID + "/" + cmdID
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

// ClipboardWriteSubject is where the daemon asks an attached terminal UI to put
// text on the user's clipboard (`##board copy`).
//
// The daemon cannot write the clipboard that matters: over SSH its own
// clipboard is the remote host's, and only the TUI owns the tty that OSC 52
// travels down to the user's local terminal. So the command's OUTPUT is made in
// the daemon — the recorder query, the selection, the text — and only the last
// hop is handed to the TUI, on this subject, as a REQUEST. A TUI that wrote the
// clipboard answers; no answer (web-only or `rysh send` sessions) sends the
// daemon to its local clipboard instead, and the confirmation says which path
// the copy took.
func ClipboardWriteSubject() string { return T("ws", "clipboard", "write") }

// MsgClipboardWrite asks an attached TUI to copy Text to the clipboard, and is
// answered with a MsgCLIResponse (OK, Output naming the path taken).
type MsgClipboardWrite struct {
	PaneID string `json:"pane_id"` // the pane the command was typed in
	Text   string `json:"text"`
}
