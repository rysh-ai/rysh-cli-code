// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"testing"
)

// E17 M1 read-state: the inbox listing carries each message's \Seen flag, and
// the UI can ask the humanoid to set or clear it.

func TestEmailSummaryCarriesSeen(t *testing.T) {
	b, _ := json.Marshal(EmailSummary{UID: 1, Seen: true})
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["seen"] != true {
		t.Fatalf("EmailSummary JSON has no seen=true: %s", b)
	}
}

func TestEmailSetSeenCodecRoundTrip(t *testing.T) {
	reg := DefaultCodecRegistry()
	in := &MsgHumanoidEmailSetSeen{UID: 42, Seen: false}
	if got := reg.TagOf(in); got != TagHumanoidEmailSetSeen {
		t.Fatalf("TagOf = %q, want %q (Send would fail)", got, TagHumanoidEmailSetSeen)
	}
	payload, _ := json.Marshal(&MsgHumanoidEmailSetSeen{UID: 42, Seen: true})
	dec, err := reg.Decode(TagHumanoidEmailSetSeen, payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	m, ok := dec.(*MsgHumanoidEmailSetSeen)
	if !ok || m.UID != 42 || !m.Seen {
		t.Fatalf("decoded %T %+v, want *MsgHumanoidEmailSetSeen{UID:42 Seen:true}", dec, dec)
	}
}
