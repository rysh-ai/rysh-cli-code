// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The email client's compose editor (E17 q, design 020 §5.4): a new message
// (`n`) or a reply (`c`) with editable To and Subject fields and a body with
// a real cursor — ←/→, home/end, ↑/↓ across lines — that takes a bracketed
// paste. tab / shift+tab move between fields; enter starts a new body line
// (and moves on from To/Subject); ctrl+d sends; esc cancels.
//
// Sending is not this file's business: updateEmailCompose hands the fields
// to emailComposeCmd, the one manual-compose door (MsgHumanoidEmailCompose →
// the humanoid's handleEmailCompose). Nothing here can reach the LLM's
// email_send path (design 019).

type composeField int

const (
	composeTo composeField = iota
	composeSubject
	composeBody
)

// composeEditor holds one message being written.
type composeEditor struct {
	to, subject, body []rune
	toPos, subjPos    int // cursor positions in to / subject
	pos               int // cursor position in body
	field             composeField
	inReplyTo         string // MessageID being answered; "" for a new message
}

// newComposeEditor starts a blank new message, cursor in To.
func newComposeEditor() *composeEditor { return &composeEditor{field: composeTo} }

// newReplyEditor starts a reply to d: To and Subject prefilled (and still
// editable), In-Reply-To set, cursor in the body.
func newReplyEditor(d *msgpkg.EmailDetail) *composeEditor {
	e := &composeEditor{field: composeBody, inReplyTo: d.MessageID}
	e.to, e.subject = []rune(d.From), []rune(replySubject(d.Subject))
	e.toPos, e.subjPos = len(e.to), len(e.subject)
	return e
}

func (e *composeEditor) To() string        { return strings.TrimSpace(string(e.to)) }
func (e *composeEditor) Subject() string   { return strings.TrimSpace(string(e.subject)) }
func (e *composeEditor) Body() string      { return string(e.body) }
func (e *composeEditor) InReplyTo() string { return e.inReplyTo }
func (e *composeEditor) IsReply() bool     { return e.inReplyTo != "" }

// line returns the focused single-line field and its cursor.
func (e *composeEditor) line() (*[]rune, *int) {
	if e.field == composeSubject {
		return &e.subject, &e.subjPos
	}
	return &e.to, &e.toPos
}

// insert puts text at the cursor of the focused field. Single-line fields
// turn line breaks into spaces.
func (e *composeEditor) insert(text string) {
	if e.field == composeBody {
		rs := []rune(text)
		e.body = append(e.body[:e.pos], append(rs, e.body[e.pos:]...)...)
		e.pos += len(rs)
		return
	}
	text = strings.NewReplacer("\n", " ").Replace(text)
	buf, p := e.line()
	rs := []rune(text)
	*buf = append((*buf)[:*p], append(rs, (*buf)[*p:]...)...)
	*p += len(rs)
}

// cleanPaste normalises a bracketed paste: CRLF/CR → LF, tab → space, every
// other control character (escape sequences included) dropped — the same
// reasoning as sanitizePastedText: control bytes would wreck the dock layout.
func cleanPaste(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", " ").Replace(s)
	return strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// lineBounds returns the start and end (exclusive of '\n') of the body line
// holding position p.
func (e *composeEditor) lineBounds(p int) (int, int) {
	start := p
	for start > 0 && e.body[start-1] != '\n' {
		start--
	}
	end := p
	for end < len(e.body) && e.body[end] != '\n' {
		end++
	}
	return start, end
}

// vertical moves the body cursor one line up (dir -1) or down (+1), keeping
// the column where the target line is long enough.
func (e *composeEditor) vertical(dir int) {
	start, end := e.lineBounds(e.pos)
	col := e.pos - start
	if dir < 0 {
		if start == 0 {
			return
		}
		ps, pe := e.lineBounds(start - 1)
		e.pos = min(ps+col, pe)
		return
	}
	if end >= len(e.body) {
		return
	}
	ns, ne := e.lineBounds(end + 1)
	e.pos = min(ns+col, ne)
}

// handleKey edits the message for one key. It reports send (ctrl+d) and
// cancel (esc); everything else is editing.
func (e *composeEditor) handleKey(msg tea.KeyMsg) (send, cancel bool) {
	switch msg.Type {
	case tea.KeyEsc:
		return false, true
	case tea.KeyCtrlD:
		return true, false
	case tea.KeyTab:
		e.field = (e.field + 1) % 3
	case tea.KeyShiftTab:
		e.field = (e.field + 2) % 3
	case tea.KeyEnter:
		if e.field == composeBody {
			e.insert("\n")
		} else {
			e.field++
		}
	case tea.KeySpace:
		e.insert(" ")
	case tea.KeyRunes:
		text := string(msg.Runes)
		if msg.Paste {
			text = cleanPaste(text)
		}
		e.insert(text)
	case tea.KeyBackspace:
		if e.field == composeBody {
			if e.pos > 0 {
				e.body = append(e.body[:e.pos-1], e.body[e.pos:]...)
				e.pos--
			}
		} else if buf, p := e.line(); *p > 0 {
			*buf = append((*buf)[:*p-1], (*buf)[*p:]...)
			*p--
		}
	case tea.KeyDelete:
		if e.field == composeBody {
			if e.pos < len(e.body) {
				e.body = append(e.body[:e.pos], e.body[e.pos+1:]...)
			}
		} else if buf, p := e.line(); *p < len(*buf) {
			*buf = append((*buf)[:*p], (*buf)[*p+1:]...)
		}
	case tea.KeyLeft:
		if e.field == composeBody {
			e.pos = max(0, e.pos-1)
		} else if _, p := e.line(); *p > 0 {
			*p--
		}
	case tea.KeyRight:
		if e.field == composeBody {
			e.pos = min(len(e.body), e.pos+1)
		} else if buf, p := e.line(); *p < len(*buf) {
			*p++
		}
	case tea.KeyHome, tea.KeyCtrlA:
		if e.field == composeBody {
			e.pos, _ = e.lineBounds(e.pos)
		} else {
			_, p := e.line()
			*p = 0
		}
	case tea.KeyEnd, tea.KeyCtrlE:
		if e.field == composeBody {
			_, e.pos = e.lineBounds(e.pos)
		} else {
			buf, p := e.line()
			*p = len(*buf)
		}
	case tea.KeyUp:
		if e.field == composeBody {
			e.vertical(-1)
		} else if e.field > composeTo {
			e.field--
		}
	case tea.KeyDown:
		if e.field == composeBody {
			e.vertical(1)
		} else {
			e.field++
		}
	}
	return false, false
}

// withCursor renders rs with a cursor bar at p when focused.
func withCursor(rs []rune, p int, focused bool) string {
	if !focused {
		return string(rs)
	}
	return string(rs[:p]) + "▏" + string(rs[p:])
}

// lines renders the editor for the dock: To, Subject, a rule, then the body.
func (e *composeEditor) lines() []string {
	out := []string{
		"To:   " + withCursor(e.to, e.toPos, e.field == composeTo),
		"Subj: " + withCursor(e.subject, e.subjPos, e.field == composeSubject),
		"──",
	}
	return append(out, strings.Split(withCursor(e.body, e.pos, e.field == composeBody), "\n")...)
}
