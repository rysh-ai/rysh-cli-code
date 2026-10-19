// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 folder rail, IMAP half: list the account's selectable folders (with
// their RFC 6154 SPECIAL-USE role) and select any of them by name.

const listFoldersResp = "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n" +
	"* LIST (\\HasChildren \\Noselect) \"/\" \"[Gmail]\"\r\n" +
	"* LIST (\\HasNoChildren \\Sent) \"/\" \"[Gmail]/Sent Mail\"\r\n" +
	"* LIST (\\HasNoChildren \\Drafts) \"/\" \"[Gmail]/Drafts\"\r\n" +
	"* LIST (\\HasNoChildren) \".\" Work.Clients\r\n" +
	"A0001 OK LIST completed\r\n"

func TestIMAPListFolders(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{listFoldersResp})
	got, err := c.listFolders()
	if err != nil {
		t.Fatalf("listFolders: %v", err)
	}
	if cmds := sent(); len(cmds) != 1 || cmds[0] != `A0001 LIST "" "*"` {
		t.Fatalf("sent %v, want [A0001 LIST \"\" \"*\"]", cmds)
	}
	want := []msg.EmailFolder{
		{Name: "INBOX", Label: "INBOX", Role: "inbox"},
		{Name: "[Gmail]/Sent Mail", Label: "Sent", Role: "sent"},
		{Name: "[Gmail]/Drafts", Label: "Drafts", Role: "drafts"},
		{Name: "Work.Clients", Label: "Clients", Role: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("folders = %+v, want %+v (\\Noselect [Gmail] must be skipped)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("folder[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestIMAPSelectMailboxQuotes(t *testing.T) {
	cases := []struct{ folder, want string }{
		{"", `A0001 SELECT "INBOX"`},
		{"[Gmail]/Sent Mail", `A0001 SELECT "[Gmail]/Sent Mail"`},
		{`odd"name\x`, `A0001 SELECT "odd\"name\\x"`},
	}
	for _, tc := range cases {
		c, sent := recordingIMAPServer(t, []string{"A0001 OK [READ-WRITE] SELECT completed\r\n"})
		if err := c.selectMailbox(tc.folder); err != nil {
			t.Fatalf("selectMailbox(%q): %v", tc.folder, err)
		}
		if cmds := sent(); len(cmds) != 1 || cmds[0] != tc.want {
			t.Errorf("selectMailbox(%q) sent %v, want [%s]", tc.folder, cmds, tc.want)
		}
	}
}
