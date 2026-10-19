// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"testing"
)

// E17 M1 folder rail: a folder listing request/reply, and every per-message
// request names the folder it acts in (IMAP UIDs are per mailbox).
func TestEmailFolderMessagesRoundTrip(t *testing.T) {
	reg := DefaultCodecRegistry()
	cases := []struct {
		tag string
		msg interface{}
	}{
		{TagHumanoidEmailFolders, &MsgHumanoidEmailFolders{}},
		{TagHumanoidEmailFoldersReply, &MsgHumanoidEmailFoldersReply{
			HumanoidName: "bot", Folders: []EmailFolder{{Name: "[Gmail]/Sent Mail", Label: "Sent", Role: "sent"}},
		}},
	}
	for _, c := range cases {
		if got := reg.TagOf(c.msg); got != c.tag {
			t.Fatalf("TagOf(%T) = %q, want %q", c.msg, got, c.tag)
		}
		payload, _ := json.Marshal(c.msg)
		if _, err := reg.Decode(c.tag, payload); err != nil {
			t.Fatalf("decode %s: %v", c.tag, err)
		}
	}
	for _, m := range []interface{}{
		&MsgHumanoidEmailList{Folder: "Sent"},
		&MsgHumanoidEmailListReply{Folder: "Sent"},
		&MsgHumanoidEmailRead{Folder: "Sent"},
		&MsgHumanoidEmailSetSeen{Folder: "Sent"},
	} {
		b, _ := json.Marshal(m)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if got["folder"] != "Sent" {
			t.Errorf("%T JSON %s has no folder", m, b)
		}
	}
}
