// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"testing"
)

func TestEmailSaveAttachmentRoundTrip(t *testing.T) {
	reg := DefaultCodecRegistry()
	for _, c := range []struct {
		tag string
		msg interface{}
	}{
		{TagHumanoidEmailSaveAttachment, &MsgHumanoidEmailSaveAttachment{Folder: "Work", UID: 7, Index: 1}},
		{TagHumanoidEmailSaveAttachmentReply, &MsgHumanoidEmailSaveAttachmentReply{HumanoidName: "bot", UID: 7, Index: 1, Path: "/tmp/x.pdf"}},
	} {
		if got := reg.TagOf(c.msg); got != c.tag {
			t.Fatalf("TagOf(%T) = %q, want %q", c.msg, got, c.tag)
		}
		b, _ := json.Marshal(c.msg)
		if _, err := reg.Decode(c.tag, b); err != nil {
			t.Fatalf("decode %s: %v", c.tag, err)
		}
	}
}
