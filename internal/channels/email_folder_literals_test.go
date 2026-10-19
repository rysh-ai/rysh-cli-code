// SPDX-License-Identifier: Apache-2.0

package channels

import "testing"

// E17 M1 (c2): a server may send a folder name as an IMAP literal ({n} then n
// bytes on the following line) instead of a quoted string — RFC 3501 allows
// it for any name and servers use it for names with quotes or 8-bit bytes.
// listFolders used to skip these, so the folder never appeared in the rail.
const listLiteralResp = "* LIST (\\HasNoChildren) \"/\" {9}\r\n" +
	"My Folder\r\n" +
	"* LIST (\\HasNoChildren) \"/\" {8}\r\n" +
	"say \"hi\"\r\n" +
	"* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n" +
	"A0001 OK LIST completed\r\n"

func TestIMAPListFoldersReadsLiteralNames(t *testing.T) {
	c, _ := recordingIMAPServer(t, []string{listLiteralResp})
	got, err := c.listFolders()
	if err != nil {
		t.Fatalf("listFolders: %v", err)
	}
	names := map[string]bool{}
	for _, f := range got {
		names[f.Name] = true
	}
	for _, want := range []string{"My Folder", `say "hi"`, "INBOX"} {
		if !names[want] {
			t.Errorf("folder %q missing from %+v: a literal-encoded name is invisible to the user", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d folders %+v, want 3 (the literal's data line must not become a folder of its own)", len(got), got)
	}
	for _, f := range got {
		if f.Name == "My Folder" && f.Label != "My Folder" {
			t.Errorf("label = %q, want \"My Folder\"", f.Label)
		}
	}
}

// TestIMAPSelectLiteralNamedFolder: a folder that arrived as a literal can be
// selected by the name listFolders returned.
func TestIMAPSelectLiteralNamedFolder(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"My Folder", `A0001 SELECT "My Folder"`},
		{`say "hi"`, `A0001 SELECT "say \"hi\""`},
	} {
		c, sent := recordingIMAPServer(t, []string{"A0001 OK SELECT completed\r\n"})
		if err := c.selectMailbox(tc.name); err != nil {
			t.Fatalf("selectMailbox(%q): %v", tc.name, err)
		}
		if cmds := sent(); len(cmds) != 1 || cmds[0] != tc.want {
			t.Errorf("selectMailbox(%q) sent %v, want [%s]", tc.name, cmds, tc.want)
		}
	}
}
