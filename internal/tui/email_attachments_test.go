// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 attachments: the reader lists an open mail's attachments and s
// saves them (receive side only — nothing here sends one).

func attachModel(t *testing.T) (Model, *emailViewState) {
	t.Helper()
	m, ev, _ := emailSearchModel(t)
	ev.folder = "Work"
	ev.detail = &msgpkg.EmailDetail{UID: 7, From: "a@x", Subject: "files", Body: "see attached",
		Attachments: []msgpkg.AttachmentInfo{
			{Filename: "report.pdf", ContentType: "application/pdf", Size: 123456},
			{Filename: "notes.txt", ContentType: "text/plain", Size: 12},
		}}
	ev.focusCol = emailColReader
	return m, ev
}

func TestEmailReaderListsAttachments(t *testing.T) {
	m, ev := attachModel(t)
	out := ansi.Strip(strings.Join(m.emailReaderRows(ev, 60, 20), "\n"))
	for _, want := range []string{"report.pdf", "121 KB", "notes.txt", "12 B"} {
		if !strings.Contains(out, want) {
			t.Errorf("reader does not show %q:\n%s", want, out)
		}
	}
}

func TestEmailSaveAttachmentsKey(t *testing.T) {
	m, ev := attachModel(t)
	_, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if cmd == nil {
		t.Fatal("s on a mail with attachments fired no save request")
	}
	if !strings.Contains(ev.status, "saving 2") {
		t.Errorf("status = %q, want it to say 2 attachments are being saved", ev.status)
	}
	if r := emailSaveAttachmentRequest(ev.folder, 7, 1); r.Folder != "Work" || r.UID != 7 || r.Index != 1 {
		t.Fatalf("emailSaveAttachmentRequest = %+v", r)
	}
	if bar := m.emailActionBar(ev); !strings.Contains(bar, "s save") {
		t.Errorf("action bar %q does not name the s key", bar)
	}

	ev.detail.Attachments = nil
	ev.status = ""
	if _, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}); cmd != nil {
		t.Fatal("s on a mail without attachments fired a request")
	}
	if !strings.Contains(ev.status, "no attachments") {
		t.Errorf("status = %q, want \"no attachments\"", ev.status)
	}
	if bar := m.emailActionBar(ev); strings.Contains(bar, "s save") {
		t.Errorf("action bar %q offers s with nothing to save", bar)
	}
}

func TestEmailSavedEventReportsPath(t *testing.T) {
	m, ev := attachModel(t)
	m.applyEmailEvent(emailEvent{kind: emailEvtSaved, humanoid: "mailbot", path: "/home/u/Downloads/report.pdf"})
	if !strings.Contains(ev.status, "/home/u/Downloads/report.pdf") {
		t.Fatalf("status = %q, want the saved path", ev.status)
	}
	m.applyEmailEvent(emailEvent{kind: emailEvtSaved, humanoid: "mailbot", err: "disk full"})
	if !strings.Contains(ev.status, "disk full") {
		t.Fatalf("status = %q, want the save error", ev.status)
	}
}
