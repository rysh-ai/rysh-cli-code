// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (r): d deletes and e archives the selected (or open) mail, after a
// one-keystroke y/n confirmation. Any key but y cancels and sends nothing.
// The mail leaves the list only when the daemon confirms the move.

func moveModel(t *testing.T) (Model, *emailViewState) {
	t.Helper()
	m, ev, _ := emailSearchModel(t)
	ev.folder = "Work"
	ev.list = []msgpkg.EmailSummary{
		{UID: 1, Subject: "first", Seen: true},
		{UID: 2, Subject: "second", Seen: true},
		{UID: 3, Subject: "third", Seen: true},
	}
	ev.selected = 1
	return m, ev
}

func TestEmailDeleteNeedsConfirmation(t *testing.T) {
	m, ev := moveModel(t)
	if _, cmd := m.updateEmailMode(key("d")); cmd != nil {
		t.Fatal("d sent something before confirmation")
	}
	if bar := m.emailActionBar(ev); !strings.Contains(bar, "Delete 'second'? y/n") {
		t.Fatalf("action bar %q does not ask to confirm", bar)
	}
	// Anything but y cancels — and sends nothing.
	if _, cmd := m.updateEmailMode(key("n")); cmd != nil {
		t.Fatal("declining the confirmation sent a request")
	}
	if len(ev.list) != 3 || ev.answer == answerCompose {
		t.Fatalf("declined delete changed state: list=%d answer=%v (n must not open compose here)", len(ev.list), ev.answer)
	}
	if !strings.Contains(ev.status, "cancel") {
		t.Fatalf("status = %q, want it to say the delete was cancelled", ev.status)
	}
}

func TestEmailDeleteAndArchiveSendOneRequest(t *testing.T) {
	m, ev := moveModel(t)
	toHumanoid, toWorkspace := composeHarness(t, &m)

	for _, tc := range []struct{ key, action string }{{"d", "delete"}, {"e", "archive"}} {
		m.updateEmailMode(key(tc.key))
		_, cmd := m.updateEmailMode(key("y"))
		if cmd == nil {
			t.Fatalf("%s confirmed but nothing sent", tc.action)
		}
		cmd()
		select {
		case d := <-toHumanoid:
			r, ok := d.(*msgpkg.MsgHumanoidEmailMove)
			if !ok || r.UID != 2 || r.Folder != "Work" || r.Action != tc.action {
				t.Fatalf("%s sent %T %+v, want move of UID 2 in Work", tc.action, d, d)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s request never reached the humanoid", tc.action)
		}
		if len(ev.list) != 3 {
			t.Fatalf("%s removed the mail before the daemon confirmed it", tc.action)
		}
	}
	select {
	case d := <-toWorkspace:
		t.Fatalf("a move also went to the workspace inbox: %T", d)
	case <-time.After(150 * time.Millisecond):
	}
	if r := emailMoveRequest("Work", 2, "delete"); r.UID != 2 || r.Folder != "Work" || r.Action != "delete" {
		t.Fatalf("emailMoveRequest = %+v", r)
	}
}

func TestEmailMoveReplyUpdatesList(t *testing.T) {
	m, ev := moveModel(t)
	m.applyEmailEvent(emailEvent{kind: emailEvtMoved, humanoid: "mailbot", folder: "Work", uid: 2, target: "Trash", action: "delete"})
	if len(ev.list) != 2 || ev.list[0].UID != 1 || ev.list[1].UID != 3 {
		t.Fatalf("list after move = %+v, want UID 2 gone", ev.list)
	}
	if ev.selected != 1 || ev.list[ev.selected].UID != 3 {
		t.Fatalf("selection = %d, want the next mail (UID 3)", ev.selected)
	}
	if !strings.Contains(ev.status, "moved to Trash") {
		t.Fatalf("status = %q", ev.status)
	}
	// The last mail: selection falls back to the previous one.
	m.applyEmailEvent(emailEvent{kind: emailEvtMoved, humanoid: "mailbot", folder: "Work", uid: 3, target: "Archive", action: "archive"})
	if len(ev.list) != 1 || ev.selected != 0 {
		t.Fatalf("after moving the last mail: list=%+v selected=%d", ev.list, ev.selected)
	}

	// An error leaves the list exactly as it was.
	m.applyEmailEvent(emailEvent{kind: emailEvtMoved, humanoid: "mailbot", folder: "Work", uid: 1, action: "delete", err: "no \\Trash folder"})
	if len(ev.list) != 1 || ev.list[0].UID != 1 || !strings.Contains(ev.status, "no \\Trash folder") {
		t.Fatalf("error: list=%+v status=%q", ev.list, ev.status)
	}
}

func TestEmailMoveFromTheReader(t *testing.T) {
	m, ev := moveModel(t)
	ev.selected = 0
	ev.detail = &msgpkg.EmailDetail{UID: 3, Subject: "third"}
	ev.focusCol = emailColReader
	m.updateEmailMode(key("e"))
	if bar := m.emailActionBar(ev); !strings.Contains(bar, "Archive 'third'? y/n") {
		t.Fatalf("from the reader the confirmation names %q, want the OPEN mail", bar)
	}
	m.updateEmailMode(key("x")) // cancel
	m.applyEmailEvent(emailEvent{kind: emailEvtMoved, humanoid: "mailbot", folder: "Work", uid: 3, target: "Archive", action: "archive"})
	if ev.detail != nil {
		t.Fatal("the open mail stayed in the reader after it was archived")
	}
}

func TestDeleteKeysAreTextInCompose(t *testing.T) {
	m, ev := moveModel(t)
	m.updateEmailMode(key("n"))
	pressKey(m, tea.KeyTab)
	pressKey(m, tea.KeyTab)
	typeIn(m, "de")
	if ev.editor.Body() != "de" || ev.answer != answerCompose {
		t.Fatalf("d/e in compose: body %q answer %v", ev.editor.Body(), ev.answer)
	}
}
