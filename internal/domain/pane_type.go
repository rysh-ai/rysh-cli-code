package domain

// Pane types — what a pane is INSTEAD of a shell.
//
// This is a different axis from a pane's input mode (EnabledModes:
// shell/prompt/rysh/chat/external/email/web), which decides how keystrokes are
// read on a pane that does have a shell. Conflating the two is easy and was a
// documented wrong turn in design 025 §2.1.
const (
	// PaneTypeNormal is the ordinary shell pane. It is represented by the EMPTY
	// string on the wire — the literal "normal" is never assigned anywhere, so
	// do not start writing it.
	PaneTypeNormal = ""

	// PaneTypeApproval is the ephemeral approval pane (actors/approval_pane.go).
	//
	// Careful: an approval pane OVERLOADS PaneSnapshot.GivenName to carry
	// "requestID\x1FresponseSubject" for the TUI. Anything that renders a
	// pane's name to a human must skip these panes or it will one day print a
	// NATS subject as somebody's name.
	PaneTypeApproval = "approval"

	// PaneTypeReplay is the recorded-playback pane (design 006 v2,
	// actors/workspace_replay.go).
	PaneTypeReplay = "replay"

	// PaneTypeAgentsBoard is the threaded board showing what every agent in the
	// session is doing (design 025). A founder ruling made this a pane type
	// rather than an input mode.
	PaneTypeAgentsBoard = "agents-board"
)

// IsShelllessPaneType reports whether a pane of this type must never start a
// shell/PTY.
//
// It exists so the shell-start guard is a set membership test in one place
// rather than a literal comparison scattered across the actor tree. The guard
// used to read `if p.paneType != "replay"`, which silently grew a PTY under any
// new read-only pane type added later — exactly what agents-board would have
// hit.
func IsShelllessPaneType(paneType string) bool {
	switch paneType {
	case PaneTypeReplay, PaneTypeAgentsBoard:
		return true
	default:
		// PaneTypeApproval is deliberately NOT here: an approval pane's
		// shell-less-ness is handled on its own path, and claiming it here
		// would change existing behaviour under cover of a refactor.
		return false
	}
}

// PaneCanHostAnAgent reports whether a pane is the kind of thing that could ever
// speak on the agents board, and therefore belongs in its roster.
//
// This is NOT a gate-3 restriction. Gate 3 says every claude may post, not only
// fleet members; nothing here looks at fleet meta, so a non-fleet pane counts
// exactly like a fleet one. What is excluded is pane TYPES that cannot host a
// claude at all: an approval dialog, a replay playback, and the board itself. A
// shell-less pane can never post, so listing it would put a name in the roster
// that never speaks — and a board counting itself among its own agents is the
// kind of small lie that makes a roster untrustworthy.
//
// It lives here, in domain, because BOTH the actor-side producer and the
// TUI-side roster seed must apply the same rule. Two copies of a rule is how
// F-18 happened.
func PaneCanHostAnAgent(paneType string) bool {
	return paneType != PaneTypeApproval && !IsShelllessPaneType(paneType)
}
