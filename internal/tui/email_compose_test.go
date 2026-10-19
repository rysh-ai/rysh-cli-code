// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (q), design 020 §5.4: compose was reply-only — `c` needed an open
// message, To and Subject were derived and not editable, and the body editor
// could only append and backspace. Now `n` opens a NEW message, To and
// Subject are editable fields (tab between fields), the body has a real
// cursor (←/→, home/end, ↑/↓ across lines) and takes a bracketed paste. A
// send — new or reply — still leaves through exactly one door: the manual
// compose request (MsgHumanoidEmailCompose → the humanoid's handleEmailCompose,
// which stages and approves the draft before SMTP), never the LLM's
// email_send path (design 019).

func typeIn(m Model, s string) {
	for _, r := range s {
		if r == ' ' {
			m.updateEmailMode(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func pressKey(m Model, t tea.KeyType) { m.updateEmailMode(tea.KeyMsg{Type: t}) }

func TestNewComposeHasEditableFields(t *testing.T) {
	m, ev, _ := emailSearchModel(t) // no message open
	m.updateEmailMode(key("n"))
	if ev.answer != answerCompose || ev.editor == nil {
		t.Fatal("n did not open a new message")
	}
	typeIn(m, "ana@x.io")
	pressKey(m, tea.KeyTab)
	typeIn(m, "Q4 numbers")
	pressKey(m, tea.KeyTab)
	typeIn(m, "line one")
	pressKey(m, tea.KeyEnter)
	typeIn(m, "line two")

	e := ev.editor
	if e.To() != "ana@x.io" || e.Subject() != "Q4 numbers" || e.Body() != "line one\nline two" || e.InReplyTo() != "" {
		t.Fatalf("editor = to %q subject %q body %q inReplyTo %q", e.To(), e.Subject(), e.Body(), e.InReplyTo())
	}
}

func TestComposeBodyCursorMovement(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	m.updateEmailMode(key("n"))
	pressKey(m, tea.KeyTab)
	pressKey(m, tea.KeyTab) // to the body
	typeIn(m, "abcd")
	pressKey(m, tea.KeyLeft)
	pressKey(m, tea.KeyLeft)
	typeIn(m, "X") // abXcd
	pressKey(m, tea.KeyHome)
	typeIn(m, "^") // ^abXcd
	pressKey(m, tea.KeyEnd)
	typeIn(m, "!") // ^abXcd!
	pressKey(m, tea.KeyEnter)
	typeIn(m, "xy") // line 2 "xy", cursor at col 2
	pressKey(m, tea.KeyUp)
	typeIn(m, "#") // line 1 col 2: ^a#bXcd!
	pressKey(m, tea.KeyDown)
	pressKey(m, tea.KeyEnd)
	typeIn(m, "z") // line 2: xyz
	pressKey(m, tea.KeyBackspace)
	pressKey(m, tea.KeyBackspace) // line 2: x
	if got, want := ev.editor.Body(), "^a#bXcd!\nx"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestComposePaste(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	m.updateEmailMode(key("n"))
	// Into To: a pasted newline cannot split an address field.
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ana@x.io\r\n"), Paste: true})
	pressKey(m, tea.KeyTab)
	pressKey(m, tea.KeyTab)
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p1\r\np2\x1b[31m\tend"), Paste: true})
	if got := ev.editor.To(); got != "ana@x.io" {
		t.Errorf("pasted To = %q, want the address with the newline dropped", got)
	}
	if got, want := ev.editor.Body(), "p1\np2[31m end"; got != want {
		t.Errorf("pasted body = %q, want %q (CRLF → LF, escape and tab neutralised)", got, want)
	}
}

func TestReplyComposeIsEditable(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	ev.detail = &msgpkg.EmailDetail{UID: 7, From: "bob@x.io", Subject: "budget", MessageID: "<b@x>"}
	m.updateEmailMode(key("c"))
	e := ev.editor
	if e == nil || e.To() != "bob@x.io" || e.Subject() != "Re: budget" || e.InReplyTo() != "<b@x>" {
		t.Fatalf("reply editor = %+v", e)
	}
	// Cursor starts in the body; shift+tab twice reaches To, which is editable.
	pressKey(m, tea.KeyShiftTab)
	pressKey(m, tea.KeyShiftTab)
	pressKey(m, tea.KeyEnd)
	typeIn(m, ",carol@x.io")
	if got := e.To(); got != "bob@x.io,carol@x.io" {
		t.Fatalf("To after editing = %q", got)
	}
}

// --- the send door ---------------------------------------------------------

// composeHarness gives the model a real publisher and records what reaches
// the humanoid's inbox and the workspace inbox.
func composeHarness(t *testing.T, m *Model) (humanoid <-chan interface{}, workspace <-chan interface{}) {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	codecs := msgpkg.DefaultCodecRegistry()
	m.pub = msgpkg.NewNATSPublisher(nc, codecs)
	m.workspaceInbox = msgpkg.T("ws", "inbox")
	sink := func(subject string) <-chan interface{} {
		ch := make(chan interface{}, 8)
		_, err := nc.Subscribe(subject, func(n *nats.Msg) {
			var env msgpkg.NATSEnvelope
			if json.Unmarshal(n.Data, &env) != nil {
				return
			}
			if d, err := codecs.Decode(env.TypeTag, env.Payload); err == nil {
				ch <- d
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	h, w := sink(msgpkg.T("humanoid", "mailbot", "inbox")), sink(m.workspaceInbox)
	_ = nc.Flush()
	return h, w
}

func sendCompose(t *testing.T, m Model) {
	t.Helper()
	_, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyCtrlD})
	if cmd == nil {
		t.Fatal("ctrl+d sent nothing")
	}
	cmd()
}

// TestNewAndReplyComposeShareTheSendGate: a new message and a reply both
// leave as MsgHumanoidEmailCompose to the humanoid — the manual-compose
// handler that stages + approves the draft — and NOTHING goes to the
// workspace inbox, i.e. no "send" prompt into the LLM/email_send path.
func TestNewAndReplyComposeShareTheSendGate(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	toHumanoid, toWorkspace := composeHarness(t, &m)

	m.updateEmailMode(key("n"))
	typeIn(m, "ana@x.io")
	pressKey(m, tea.KeyTab)
	typeIn(m, "hi")
	pressKey(m, tea.KeyTab)
	typeIn(m, "new message")
	sendCompose(t, m)

	ev.detail = &msgpkg.EmailDetail{UID: 7, From: "bob@x.io", Subject: "budget", MessageID: "<b@x>"}
	m.updateEmailMode(key("c"))
	typeIn(m, "a reply")
	sendCompose(t, m)

	want := []msgpkg.MsgHumanoidEmailCompose{
		{To: "ana@x.io", Subject: "hi", Body: "new message"},
		{To: "bob@x.io", Subject: "Re: budget", Body: "a reply", InReplyTo: "<b@x>"},
	}
	for i, w := range want {
		select {
		case d := <-toHumanoid:
			c, ok := d.(*msgpkg.MsgHumanoidEmailCompose)
			if !ok || *c != w {
				t.Fatalf("send %d reached the humanoid as %T %+v, want %+v", i, d, d, w)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("send %d never reached the humanoid", i)
		}
	}
	select {
	case d := <-toWorkspace:
		t.Fatalf("a compose send also went to the workspace inbox as %T %+v — that is the LLM path", d, d)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestComposeRefusesABlankRecipient(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	m.updateEmailMode(key("n"))
	pressKey(m, tea.KeyTab)
	typeIn(m, "no recipient")
	if _, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyCtrlD}); cmd != nil {
		t.Fatal("a message with no recipient was sent")
	}
	if ev.answer != answerCompose || ev.status == "" {
		t.Fatalf("blank To: answer=%v status=%q, want the editor kept open with a reason", ev.answer, ev.status)
	}
}

// TestComposeSurvivesAFailedSend: an SMTP failure reopens the editor with
// the message intact instead of losing what was typed.
func TestComposeSurvivesAFailedSend(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	m.updateEmailMode(key("n"))
	typeIn(m, "ana@x.io")
	pressKey(m, tea.KeyTab)
	pressKey(m, tea.KeyTab)
	typeIn(m, "keep me")
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyCtrlD})
	m.applyEmailEvent(emailEvent{kind: emailEvtCompose, humanoid: "mailbot", ok: false, err: "smtp: 550"})
	if ev.answer != answerCompose || ev.editor == nil || ev.editor.Body() != "keep me" {
		t.Fatalf("after a failed send: answer=%v editor=%+v, want the editor back with the text", ev.answer, ev.editor)
	}
	m.applyEmailEvent(emailEvent{kind: emailEvtCompose, humanoid: "mailbot", ok: true})
	if ev.editor != nil || ev.answer != answerNone {
		t.Fatal("a successful send left the editor open")
	}
}
