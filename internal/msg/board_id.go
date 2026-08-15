// SPDX-License-Identifier: Apache-2.0

package msg

import "strings"

// Board ids (design 028 §5, founder gate D-12 approved 2026-08-11).
//
// A BOARD IS SCOPED BY ITS SUBJECT, NEVER BY A FIELD ON MsgBoardPost. That is
// the whole of the ruling and the reason this file exists instead of a `Fleet`
// field. Founder gate 4 removed Fleet/Role/Unit from the post schema
// (messages_board.go), and gate 3 says every claude in the session may post,
// not only fleet members. Both survive because a board id is an ADDRESS:
//
//   - a post carries no board id at all — it is delivered to one;
//   - a pane with no board of its own posts to the DEFAULT board through the
//     byte-identical message shape, so there is still no field that can be
//     "missing" for a non-fleet poster.
//
// THE DEFAULT BOARD KEEPS THE THREE-TOKEN SUBJECT IT HAS ALWAYS HAD —
// "<session>.board.post", not "<session>.board.session.post". This is a
// refinement of design 028 §6.1, which proposed publishing on the four-token
// form and keeping the old one as an alias. Publishing on the OLD form for the
// default board is strictly better: the wire for the session board is
// unchanged, so a TUI or CLI built before this change still sees every post on
// it, and there is no double-publish to dedupe. Named boards are new subjects,
// and an older reader not seeing a board that did not exist when it was built
// is correct rather than a compatibility break.
//
// Anything that SUBSCRIBES therefore listens to both shapes — see
// board.Subscribe, which takes the wildcard and the legacy subject and demuxes
// on the delivered subject.

// DefaultBoardID names the session board: the one every claude can post to and
// the one a pane with no board of its own belongs to.
//
// It is a NAME, not a subject token. IsDefaultBoard is what decides the wire
// shape, and for this id the wire shape is the legacy three-token subject.
const DefaultBoardID = "session"

// MaxBoardIDLen bounds a board id. A board id becomes a SUBJECT TOKEN and,
// through internal/board, a KV key prefix; both are cheaper to keep short than
// to explain later.
const MaxBoardIDLen = 32

// NormalizeBoardID maps the several spellings of "the session board" onto one.
// An empty id, the default id itself, and any case variant of it are the same
// board. Everything else is returned lower-cased and otherwise untouched — this
// does NOT sanitise; ValidateBoardID is what rejects.
func NormalizeBoardID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return DefaultBoardID
	}
	return id
}

// IsDefaultBoard reports whether id addresses the session board.
func IsDefaultBoard(id string) bool { return NormalizeBoardID(id) == DefaultBoardID }

// ValidateBoardID rejects anything that cannot safely be a subject token.
//
// THIS IS A SUBJECT-INJECTION GUARD, not a style preference. A board id ends up
// between "board" and "post" in a NATS subject, so an id containing "." would
// silently publish into a subject nobody subscribes to, and an id of "*" or ">"
// would publish into (or subscribe to) EVERY board. Both failures are silent at
// the publisher and invisible at the board that should have received the post,
// which is why this is checked at every edge that accepts an id from a human,
// a flag, or pane meta.
func ValidateBoardID(id string) error {
	norm := NormalizeBoardID(id)
	if len(norm) > MaxBoardIDLen {
		return &BoardIDError{ID: id, Reason: "longer than 32 characters"}
	}
	for i := 0; i < len(norm); i++ {
		c := norm[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return &BoardIDError{ID: id, Reason: "may contain only a-z, 0-9, '-' and '_'"}
		}
	}
	if norm[0] == '-' || norm[0] == '_' {
		return &BoardIDError{ID: id, Reason: "must start with a letter or a digit"}
	}
	return nil
}

// BoardIDFromMeta resolves which board a PANE reads and posts to, from the
// value of its `board.id` meta.
//
// ONE PREDICATE, TWO SURFACES. The TUI renders board panes itself
// (tui.Model.boardIDForPane) and the web server answers the app's board_get
// with the same question; before this existed the TUI owned the only copy, and
// the second surface would have had to restate it. That restatement is the F-18
// shape design 025 §8b puts a rule in one predicate to avoid — and here it
// would be invisible, because both copies return a VALID board id. They would
// simply return different ones, and the app would confidently render a board
// the TUI does not show for the same pane.
//
// An empty or INVALID id is the session board rather than an error, matching
// boardSubject's fallback: a pane whose meta was hand-edited to something
// unusable still belongs somewhere, and the session board is where an
// unattributed pane belongs. Callers that took an id from a human — the CLI's
// --board, ##board open — must keep REFUSING instead; this is the pane-meta
// edge, where there is nobody to tell.
func BoardIDFromMeta(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || ValidateBoardID(id) != nil {
		return DefaultBoardID
	}
	return NormalizeBoardID(id)
}

// BoardIDError is a rejected board id, carrying what was wrong so the surface
// that took the id can say it back to whoever typed it.
type BoardIDError struct {
	ID     string
	Reason string
}

func (e *BoardIDError) Error() string {
	return "invalid board id " + quoteBoardID(e.ID) + ": " + e.Reason
}

func quoteBoardID(id string) string {
	if len(id) > MaxBoardIDLen*2 {
		id = id[:MaxBoardIDLen*2] + "…"
	}
	return "\"" + id + "\""
}

// boardSubject builds one board subject.
//
// AN INVALID ID FALLS BACK TO THE DEFAULT BOARD rather than reaching the wire.
// That is defence in depth and not the rule: every edge that accepts a board id
// validates it and refuses (see the CLI's --board, ##board open, and the pane
// meta reader), because posting to the session board when the caller named a
// fleet board is a wrong answer. It is, however, the SAFEST wrong answer — the
// session board is where an unattributed post belongs — and it is bounded,
// which "let the id become subject tokens" is not.
func boardSubject(board, leaf string) string {
	if ValidateBoardID(board) != nil || IsDefaultBoard(board) {
		return T("board", leaf)
	}
	return T("board", NormalizeBoardID(board), leaf)
}

// BoardIDFromSubject extracts the board a delivered message was addressed to.
//
// It parses from the END, never by counting from the front: T's prefix is the
// session name (rysh-shared/msg/topics.go), and a session name is free-form
// enough to contain a dot. Counting tokens from the left would then read part
// of the session name as a board id — the sort of failure that shows up as an
// empty board rather than as an error.
//
//	<session>.board.post          → ("session", true)
//	<session>.board.epic-07.post  → ("epic-07", true)
//	<session>.pane.x.output       → ("", false)
func BoardIDFromSubject(subject, leaf string) (string, bool) {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 || parts[len(parts)-1] != leaf {
		return "", false
	}
	switch parts[len(parts)-2] {
	case "board":
		return DefaultBoardID, true
	default:
		if len(parts) < 4 || parts[len(parts)-3] != "board" {
			return "", false
		}
		id := parts[len(parts)-2]
		if ValidateBoardID(id) != nil {
			return "", false
		}
		return NormalizeBoardID(id), true
	}
}
