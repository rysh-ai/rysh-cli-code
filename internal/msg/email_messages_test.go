package msg

import (
	"encoding/json"
	"testing"
)

// TestEmailMessageCodecRoundTrip verifies every new email message type is
// registered in the default codec registry. TagOf must resolve (otherwise
// NATSPublisher.Send fails with "unregistered message type") and Decode must
// reconstruct the value (otherwise the web-server bridge drops the event).
func TestEmailMessageCodecRoundTrip(t *testing.T) {
	reg := DefaultCodecRegistry()
	cases := []struct {
		name string
		tag  string
		msg  interface{}
	}{
		{"list", TagHumanoidEmailList, &MsgHumanoidEmailList{Count: 5, Search: "hi"}},
		{"read", TagHumanoidEmailRead, &MsgHumanoidEmailRead{UID: 42}},
		{"listReply", TagHumanoidEmailListReply, &MsgHumanoidEmailListReply{
			HumanoidName: "bot",
			Emails:       []EmailSummary{{UID: 1, MessageID: "<a@x>", InReplyTo: "<b@x>"}},
		}},
		{"readReply", TagHumanoidEmailReadReply, &MsgHumanoidEmailReadReply{
			HumanoidName: "bot", Email: &EmailDetail{UID: 1, Subject: "s"},
		}},
		{"changed", TagHumanoidEmailChanged, &MsgHumanoidEmailChanged{HumanoidName: "bot"}},
		{"focus", TagHumanoidSetFocus, &MsgHumanoidSetFocus{
			UID: 9, MessageID: "<m@x>", From: "Alice <a@x>", Subject: "Hi", Body: "hello",
		}},
		{"focusListing", TagHumanoidSetFocus, &MsgHumanoidSetFocus{Listing: true}},
	}
	for _, c := range cases {
		if got := reg.TagOf(c.msg); got != c.tag {
			t.Fatalf("%s: TagOf = %q, want %q (Send would fail)", c.name, got, c.tag)
		}
		payload, err := json.Marshal(c.msg)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		decoded, err := reg.Decode(c.tag, payload)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if decoded == nil {
			t.Fatalf("%s: decode returned nil", c.name)
		}
	}

	// Spot-check that the listReply round-trips its payload (not just its tag).
	rt, _ := json.Marshal(&MsgHumanoidEmailListReply{
		HumanoidName: "bot",
		Emails:       []EmailSummary{{UID: 7, From: "a@x", MessageID: "<m@x>"}},
	})
	dec, err := reg.Decode(TagHumanoidEmailListReply, rt)
	if err != nil {
		t.Fatalf("listReply decode: %v", err)
	}
	r, ok := dec.(*MsgHumanoidEmailListReply)
	if !ok {
		t.Fatalf("listReply decoded to %T, want *MsgHumanoidEmailListReply", dec)
	}
	if len(r.Emails) != 1 || r.Emails[0].UID != 7 || r.Emails[0].MessageID != "<m@x>" {
		t.Fatalf("listReply payload not preserved: %+v", r)
	}
}

// TestEmailSummaryThreadingFields ensures EmailSummary serializes the threading
// identifiers the desktop UI needs to thread messages and set reply In-Reply-To.
func TestEmailSummaryThreadingFields(t *testing.T) {
	b, _ := json.Marshal(EmailSummary{UID: 1, MessageID: "<m@x>", InReplyTo: "<p@x>"})
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["message_id"] != "<m@x>" || got["in_reply_to"] != "<p@x>" {
		t.Fatalf("EmailSummary JSON missing threading fields: %s", b)
	}
}
