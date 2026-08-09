package actors

import (
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// agents-board persona registration (design 025 §3.3, epic §4.1).
//
// A pane announces itself so the board can show a roster before the pane's
// first post. This is the producer whose absence made registration UNWIRED:
// the type, subject, publisher, subscriber, store and view all existed, and
// nothing ever called SendBoardRegister outside tests.
//
// It lives on the PANE and not on the board: a pane knows its own identity, and
// the board is a listener. Registration is advisory in both directions — the
// board renders posts from panes that never registered (board.Store), and a
// pane that registers into a session with no board loses nothing.

// registerOnBoard announces this pane's persona.
//
// Fire-and-forget, exactly like the other best-effort pane publishers: a board
// that misses an announcement still renders that pane's posts, so a failure
// here must never be worth failing pane startup over.
func (p *PaneActor) registerOnBoard() {
	if !paneCanHostAnAgent(p.paneType) {
		return
	}
	// Through BoardPersona, never the raw given-name — the same rule the post
	// path uses (workspace_board.go buildBoardPost), not a second one. The
	// roster is a SECOND surface that could otherwise print an approval pane's
	// "requestID\x1FresponseSubject" as somebody's name.
	persona := msg.BoardPersona(p.givenName, p.title, p.id)
	_ = msg.SendBoardRegister(p.pub, msg.NewBoardRegister(p.id, persona, time.Now().UnixMilli()))
}

// paneCanHostAnAgent delegates to domain so the actor producer and the TUI
// roster seed cannot drift apart. See domain.PaneCanHostAnAgent.
func paneCanHostAnAgent(paneType string) bool {
	return domain.PaneCanHostAnAgent(paneType)
}
