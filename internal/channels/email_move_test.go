// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"strings"
	"testing"
)

// E17 (r), design 020 §5.5: delete and archive MOVE a mail to the account's
// SPECIAL-USE folder — \Trash for delete, \Archive (else Gmail's \All) for
// archive. Safe by default: never a plain EXPUNGE, never \Deleted in place,
// never a guessed or created folder. UID MOVE (RFC 6851) when advertised;
// else COPY + STORE \Deleted + UID EXPUNGE of that ONE uid (UIDPLUS); else
// refuse. The command order is CAPABILITY, LIST (find the role folder),
// SELECT the source folder, then the move.

const capMove = "* CAPABILITY IMAP4rev1 MOVE UIDPLUS\r\nA0001 OK CAPABILITY completed\r\n"
const capUIDPlus = "* CAPABILITY IMAP4rev1 UIDPLUS\r\nA0001 OK CAPABILITY completed\r\n"
const capNeither = "* CAPABILITY IMAP4rev1\r\nA0001 OK CAPABILITY completed\r\n"

const listStd = "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n" +
	"* LIST (\\HasNoChildren) \"/\" \"Work\"\r\n" +
	"* LIST (\\HasNoChildren \\Trash) \"/\" \"Deleted Items\"\r\n" +
	"* LIST (\\HasNoChildren \\Archive) \"/\" \"Archive\"\r\n" +
	"A0002 OK LIST completed\r\n"

const listGmail = "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n" +
	"* LIST (\\HasChildren \\Noselect) \"/\" \"[Gmail]\"\r\n" +
	"* LIST (\\HasNoChildren \\All) \"/\" \"[Gmail]/All Mail\"\r\n" +
	"* LIST (\\HasNoChildren \\Trash) \"/\" \"[Gmail]/Trash\"\r\n" +
	"A0002 OK LIST completed\r\n"

const listNoRoles = "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n" +
	"* LIST (\\HasNoChildren) \"/\" \"Trash\"\r\n" + // a folder NAMED Trash, without the \Trash role
	"A0002 OK LIST completed\r\n"

const selectOK = "* 3 EXISTS\r\nA0003 OK [READ-WRITE] SELECT completed\r\n"

// mutating reports whether any command that changes the mailbox was sent.
func mutating(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		u := strings.ToUpper(c)
		for _, verb := range []string{" UID MOVE ", " UID COPY ", " UID STORE ", " UID EXPUNGE", " EXPUNGE", " STORE ", " COPY ", " MOVE ", " CREATE "} {
			if strings.Contains(u, verb) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func TestMoveToRoleUsesUIDMove(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capMove, listStd, selectOK, "A0004 OK MOVE completed\r\n"})
	target, err := c.moveToRole("Work", 7, "delete")
	if err != nil || target != "Deleted Items" {
		t.Fatalf("moveToRole = %q, %v; want the \\Trash folder \"Deleted Items\"", target, err)
	}
	want := []string{`A0001 CAPABILITY`, `A0002 LIST "" "*"`, `A0003 SELECT "Work"`, `A0004 UID MOVE 7 "Deleted Items"`}
	if got := sent(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q\nwant %q", got, want)
	}
}

func TestMoveToRoleFallsBackToCopyStoreUIDExpunge(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capUIDPlus, listStd, selectOK,
		"A0004 OK COPY completed\r\n", "A0005 OK STORE completed\r\n", "A0006 OK EXPUNGE completed\r\n"})
	target, err := c.moveToRole("INBOX", 9, "archive")
	if err != nil || target != "Archive" {
		t.Fatalf("moveToRole = %q, %v", target, err)
	}
	want := []string{`A0001 CAPABILITY`, `A0002 LIST "" "*"`, `A0003 SELECT "INBOX"`,
		`A0004 UID COPY 9 "Archive"`, `A0005 UID STORE 9 +FLAGS.SILENT (\Deleted)`, `A0006 UID EXPUNGE 9`}
	if got := sent(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %q\nwant %q", got, want)
	}
}

func TestMoveToRoleCopyFailureTouchesNothing(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capUIDPlus, listStd, selectOK, "A0004 NO [OVERQUOTA] archive is full\r\n"})
	if _, err := c.moveToRole("INBOX", 9, "archive"); err == nil {
		t.Fatal("a failed COPY was reported as success")
	}
	for _, cmd := range sent() {
		if u := strings.ToUpper(cmd); strings.Contains(u, "STORE") || strings.Contains(u, "EXPUNGE") {
			t.Fatalf("after COPY failed, %q was still sent: the source would be flagged/removed", cmd)
		}
	}
}

func TestMoveToRoleRefusesWithoutMoveOrUIDPlus(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capNeither, listStd, selectOK})
	_, err := c.moveToRole("INBOX", 9, "delete")
	if err == nil || !strings.Contains(err.Error(), "MOVE") {
		t.Fatalf("err = %v, want a refusal naming MOVE/UIDPLUS", err)
	}
	if m := mutating(sent()); len(m) != 0 {
		t.Fatalf("refused, but sent %q", m)
	}
}

func TestMoveToRoleRefusesWithoutTheRoleFolder(t *testing.T) {
	for _, action := range []string{"delete", "archive"} {
		c, sent := recordingIMAPServer(t, []string{capMove, listNoRoles, selectOK, "A0004 OK\r\n"})
		_, err := c.moveToRole("INBOX", 9, action)
		if err == nil {
			t.Fatalf("%s with no role folder succeeded (a folder merely NAMED Trash must not be guessed)", action)
		}
		if m := mutating(sent()); len(m) != 0 {
			t.Fatalf("%s refused, but sent %q", action, m)
		}
	}
}

func TestArchiveOnGmailGoesToAllMail(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capMove, listGmail, selectOK, "A0004 OK MOVE completed\r\n"})
	target, err := c.moveToRole("INBOX", 4, "archive")
	if err != nil || target != "[Gmail]/All Mail" {
		t.Fatalf("Gmail archive = %q, %v; want [Gmail]/All Mail", target, err)
	}
	if got := sent(); got[len(got)-1] != `A0004 UID MOVE 4 "[Gmail]/All Mail"` {
		t.Fatalf("sent %q", got)
	}
}

func TestMoveToRoleRefusesWhenAlreadyThere(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capMove, listStd, selectOK, "A0004 OK\r\n"})
	_, err := c.moveToRole("Deleted Items", 5, "delete")
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("delete from Trash: err = %v, want an \"already in\" refusal (no permanent delete in this slice)", err)
	}
	if m := mutating(sent()); len(m) != 0 {
		t.Fatalf("refused, but sent %q", m)
	}
}

func TestMoveToRoleRejectsUnknownAction(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{capMove, listStd, selectOK, "A0004 OK\r\n"})
	if _, err := c.moveToRole("INBOX", 5, "expunge"); err == nil {
		t.Fatal("an unknown action was accepted")
	}
	if m := mutating(sent()); len(m) != 0 {
		t.Fatalf("sent %q", m)
	}
}
