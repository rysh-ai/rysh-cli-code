// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (m): the humanoid's email-client handlers (list, read, compose,
// set-seen, folders, save-attachment) had no tests — they took the concrete
// *channels.EmailAdapter, which dials TLS. They now reach the mailbox through
// the narrow emailMailbox interface, so a fake drives them: what each handler
// asks the mailbox for, and what it publishes back to the UI.

type fakeMailbox struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeMailbox) record(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}
func (f *fakeMailbox) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeMailbox) ListEmailsIn(folder string, count int, search string) ([]msg.EmailSummary, error) {
	err := f.record("list " + folder + " " + search)
	return []msg.EmailSummary{{UID: 1, Subject: "hello"}}, err
}
func (f *fakeMailbox) ReadEmailIn(folder string, uid int) (*msg.EmailDetail, error) {
	err := f.record("read " + folder)
	return &msg.EmailDetail{UID: uid, Subject: "hello"}, err
}
func (f *fakeMailbox) SendEmail(to, subject, body, inReplyTo string, _ []channels.Attachment) error {
	return f.record("send " + to + "|" + subject + "|" + body + "|" + inReplyTo)
}
func (f *fakeMailbox) SetSeen(folder string, uid int, seen bool) error {
	s := "unseen"
	if seen {
		s = "seen"
	}
	return f.record("setseen " + folder + " " + s)
}
func (f *fakeMailbox) ListFolders() ([]msg.EmailFolder, error) {
	err := f.record("folders")
	return []msg.EmailFolder{{Name: "INBOX", Label: "INBOX", Role: "inbox"}}, err
}
func (f *fakeMailbox) MoveToRole(folder string, uid int, action string) (string, error) {
	err := f.record("move " + folder + " " + action)
	return "Trash", err
}
func (f *fakeMailbox) SaveAttachment(folder string, uid, index int, dir string) (string, error) {
	err := f.record("save " + folder + " " + dir)
	return dir + "/a.pdf", err
}

func handlerHumanoid(t *testing.T, mb emailMailbox) (*HumanoidActor, *nats.Conn, *msg.CodecRegistry) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	return &HumanoidActor{name: "mb", pub: msg.NewNATSPublisher(nc, codecs), mailbox: mb}, nc, codecs
}

// replyOn subscribes to humanoid.mb.email.<kind> and returns a receiver.
func replyOn(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, kind string) func() interface{} {
	t.Helper()
	sub, err := nc.SubscribeSync(msg.T("humanoid", "mb", "email", kind))
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	return func() interface{} {
		t.Helper()
		raw, err := sub.NextMsg(3 * time.Second)
		if err != nil {
			t.Fatalf("no reply on email.%s: %v", kind, err)
		}
		return decodeEnvelope(t, codecs, raw)
	}
}

func TestEmailListHandlerEchoesFolderAndErrors(t *testing.T) {
	fake := &fakeMailbox{}
	h, nc, codecs := handlerHumanoid(t, fake)
	next := replyOn(t, nc, codecs, "list")

	h.handleEmailListQuery("Work", 20, "invoice")
	r := next().(*msg.MsgHumanoidEmailListReply)
	if r.Folder != "Work" || len(r.Emails) != 1 || r.Err != "" || r.HumanoidName != "mb" {
		t.Fatalf("list reply = %+v", r)
	}
	if got := fake.got(); len(got) != 1 || got[0] != "list Work invoice" {
		t.Fatalf("mailbox asked %v, want one list of Work for \"invoice\"", got)
	}

	fake.err = errors.New("imap down")
	h.handleEmailListQuery("Work", 20, "")
	if r := next().(*msg.MsgHumanoidEmailListReply); r.Err != "imap down" || r.Folder != "Work" {
		t.Fatalf("error reply = %+v, want err \"imap down\" with the folder echoed", r)
	}
}

func TestEmailReadAndFoldersHandlers(t *testing.T) {
	fake := &fakeMailbox{}
	h, nc, codecs := handlerHumanoid(t, fake)
	detail, folders := replyOn(t, nc, codecs, "detail"), replyOn(t, nc, codecs, "folders")

	h.handleEmailReadQuery("Work", 7)
	if r := detail().(*msg.MsgHumanoidEmailReadReply); r.Email == nil || r.Email.UID != 7 || r.Err != "" {
		t.Fatalf("read reply = %+v", r)
	}
	h.handleEmailFoldersQuery()
	if r := folders().(*msg.MsgHumanoidEmailFoldersReply); len(r.Folders) != 1 || r.Folders[0].Role != "inbox" {
		t.Fatalf("folders reply = %+v", r)
	}
	if got := fake.got(); len(got) != 2 || got[0] != "read Work" || got[1] != "folders" {
		t.Fatalf("mailbox asked %v", got)
	}
}

// TestEmailSetSeenAlwaysAnnouncesAChange: success or failure, the UI is told
// to re-list, so an optimistic flip is corrected by the server's real state.
func TestEmailSetSeenAlwaysAnnouncesAChange(t *testing.T) {
	fake := &fakeMailbox{}
	h, nc, codecs := handlerHumanoid(t, fake)
	changed := replyOn(t, nc, codecs, "changed")

	h.handleEmailSetSeen("Work", 7, true)
	if _, ok := changed().(*msg.MsgHumanoidEmailChanged); !ok {
		t.Fatal("no email.changed after set-seen")
	}
	fake.err = errors.New("read-only mailbox")
	h.handleEmailSetSeen("Work", 7, false)
	if _, ok := changed().(*msg.MsgHumanoidEmailChanged); !ok {
		t.Fatal("no email.changed after a FAILED set-seen: the UI would keep its wrong optimistic flip")
	}
	if got := fake.got(); len(got) != 2 || got[0] != "setseen Work seen" || got[1] != "setseen Work unseen" {
		t.Fatalf("mailbox asked %v", got)
	}
}

func TestEmailSaveAttachmentUsesTheConfiguredDir(t *testing.T) {
	fake := &fakeMailbox{}
	h, nc, codecs := handlerHumanoid(t, fake)
	next := replyOn(t, nc, codecs, "attachment")
	dir := t.TempDir()
	h.cfg = config.Config{}
	h.cfg.Email.AttachmentDir = dir

	h.handleEmailSaveAttachment("Work", 7, 0)
	if r := next().(*msg.MsgHumanoidEmailSaveAttachmentReply); r.Path != dir+"/a.pdf" || r.Err != "" || r.UID != 7 {
		t.Fatalf("save reply = %+v", r)
	}
	if got := fake.got(); len(got) != 1 || got[0] != "save Work "+dir {
		t.Fatalf("mailbox asked %v, want a save into %s", got, dir)
	}

	// A configured directory that does not exist: refused before the mailbox.
	h.cfg.Email.AttachmentDir = dir + "/nope"
	h.handleEmailSaveAttachment("Work", 7, 0)
	if r := next().(*msg.MsgHumanoidEmailSaveAttachmentReply); !strings.Contains(r.Err, "does not exist") {
		t.Fatalf("save reply = %+v, want a does-not-exist error", r)
	}
	if got := fake.got(); len(got) != 1 {
		t.Fatalf("mailbox was asked to save into a missing directory: %v", got)
	}
}

func TestEmailComposeHandler(t *testing.T) {
	fake := &fakeMailbox{}
	h, nc, codecs := handlerHumanoid(t, fake)
	next := replyOn(t, nc, codecs, "compose")

	h.handleEmailCompose(&msg.MsgHumanoidEmailCompose{To: "  ", Subject: "s", Body: "b"})
	if r := next().(*msg.MsgHumanoidEmailComposeReply); r.Ok || r.Err != "no recipient" {
		t.Fatalf("blank recipient reply = %+v", r)
	}
	h.handleEmailCompose(&msg.MsgHumanoidEmailCompose{To: "ana@x.io", Subject: "Re: plan", Body: "ok", InReplyTo: "<m@x>"})
	if r := next().(*msg.MsgHumanoidEmailComposeReply); !r.Ok || r.Err != "" {
		t.Fatalf("send reply = %+v", r)
	}
	if got := fake.got(); len(got) != 1 || got[0] != "send ana@x.io|Re: plan|ok|<m@x>" {
		t.Fatalf("mailbox sent %v, want exactly the composed reply", got)
	}
}

// TestEmailHandlersWithoutAMailbox: a humanoid with no email channel answers
// every UI request with the same refusal — including when its adapter field
// is a nil *EmailAdapter (the interface must not hold a typed nil).
func TestEmailHandlersWithoutAMailbox(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	h := &HumanoidActor{name: "mb", pub: msg.NewNATSPublisher(nc, codecs), emailAdapter: nil}
	list, detail := replyOn(t, nc, codecs, "list"), replyOn(t, nc, codecs, "detail")
	h.handleEmailListQuery("", 10, "")
	h.handleEmailReadQuery("", 1)
	if r := list().(*msg.MsgHumanoidEmailListReply); r.Err != "no email channel configured" {
		t.Fatalf("list without a mailbox = %+v", r)
	}
	if r := detail().(*msg.MsgHumanoidEmailReadReply); r.Err != "no email channel configured" {
		t.Fatalf("read without a mailbox = %+v", r)
	}
}

// E17 (r): delete/archive moves the mail to its role folder; the reply names
// the target, and a successful move also announces email.changed.
func TestEmailMoveHandler(t *testing.T) {
	fake := &fakeMailbox{}
	h, nc, codecs := handlerHumanoid(t, fake)
	moved, changed := replyOn(t, nc, codecs, "move"), replyOn(t, nc, codecs, "changed")

	h.handleEmailMove("Work", 7, "delete")
	r := moved().(*msg.MsgHumanoidEmailMoveReply)
	if r.Target != "Trash" || r.Err != "" || r.UID != 7 || r.Folder != "Work" || r.Action != "delete" {
		t.Fatalf("move reply = %+v", r)
	}
	if _, ok := changed().(*msg.MsgHumanoidEmailChanged); !ok {
		t.Fatal("no email.changed after a move")
	}
	if got := fake.got(); len(got) != 1 || got[0] != "move Work delete" {
		t.Fatalf("mailbox asked %v", got)
	}

	fake.err = errors.New("no \\Trash folder")
	h.handleEmailMove("Work", 7, "delete")
	if r := moved().(*msg.MsgHumanoidEmailMoveReply); r.Err == "" || r.Target != "" {
		t.Fatalf("failed move reply = %+v, want an error and no target", r)
	}
}
