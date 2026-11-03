// SPDX-License-Identifier: Apache-2.0

package channels

// Matrix E2EE is deferred (root ruling 2026-10-28), so an encrypted room must
// never fail silently: the first encrypted event in a room is answered once
// with a plain m.notice saying the bot cannot read it, and Status names it —
// but only for a sender the config allows (e20 review): a stranger who
// invites the bot to an encrypted room learns nothing and uses no slot.

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func waitMatrixSends(fake *fakeplatform.Matrix, n int) []fakeplatform.MatrixSend {
	deadline := time.Now().Add(3 * time.Second)
	for len(fake.Sends()) < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let a stray extra post show up
	return fake.Sends()
}

func TestMatrixEncryptedRoomIsNeverSilent(t *testing.T) {
	const alice = "@alice:fake.local"
	fake, a, _ := startMatrix(t, msg.ChannelConfig{Allowlist: []string{alice}}, nil)
	room := fake.DMRoom(alice)

	fake.PushEncrypted(room, alice)
	noInbound(t, a) // still never forwarded: v1 cannot decrypt it
	s := waitMatrixSends(fake, 1)
	if len(s) != 1 || s[0].RoomID != room || s[0].MsgType != "m.notice" ||
		!strings.Contains(s[0].Body, "encrypted") || !strings.Contains(s[0].Body, "unencrypted") {
		t.Fatalf("want one plain m.notice explaining the limit in %s, got %+v", room, s)
	}

	fake.PushEncrypted(room, alice)
	noInbound(t, a)
	if s := waitMatrixSends(fake, 2); len(s) != 1 {
		t.Fatalf("a second encrypted event posted again: %+v", s)
	}
	if st := a.Status(); !strings.Contains(st.Details, "encrypted") || !strings.Contains(st.Details, room) {
		t.Errorf("status does not name the encrypted room: %q", st.Details)
	}
}

func TestMatrixEncryptedNoticeRespectsAllowList(t *testing.T) {
	fake, a, _ := startMatrix(t, msg.ChannelConfig{Channels: []string{"!team:fake.local"}}, nil)
	fake.PushEncrypted("!other:fake.local", "@bob:fake.local")
	noInbound(t, a)
	if s := waitMatrixSends(fake, 1); len(s) != 0 {
		t.Fatalf("posted into a room outside the channels allow-list: %+v", s)
	}
}

func TestMatrixEncryptedNoticeOnlyForAllowedSenders(t *testing.T) {
	const owner, stranger = "@owner:fake.local", "@stranger:fake.local"
	t.Run("stranger with an allowlist", func(t *testing.T) {
		fake, a, _ := startMatrix(t, msg.ChannelConfig{Allowlist: []string{owner}}, nil)
		fake.PushEncrypted("!lure:fake.local", stranger)
		noInbound(t, a)
		if s := waitMatrixSends(fake, 1); len(s) != 0 {
			t.Fatalf("a stranger's encrypted room got a notice: %+v", s)
		}
		if st := a.Status(); strings.Contains(st.Details, "encrypted room") {
			t.Errorf("a stranger's room used a slot: %q", st.Details)
		}
	})
	t.Run("neither room list nor allowlist", func(t *testing.T) {
		fake, a, _ := startMatrix(t, msg.ChannelConfig{}, nil)
		fake.PushEncrypted("!any:fake.local", stranger)
		noInbound(t, a)
		if s := waitMatrixSends(fake, 1); len(s) != 0 {
			t.Fatalf("posted with no allowlist and no room list: %+v", s)
		}
		if st := a.Status(); strings.Contains(st.Details, "encrypted room") {
			t.Errorf("a slot was used with nothing allowed: %q", st.Details)
		}
	})
	t.Run("allowed room, no allowlist", func(t *testing.T) {
		fake, a, _ := startMatrix(t, msg.ChannelConfig{Channels: []string{"!team:fake.local"}}, nil)
		fake.PushEncrypted("!team:fake.local", stranger)
		noInbound(t, a)
		if s := waitMatrixSends(fake, 1); len(s) != 1 || s[0].RoomID != "!team:fake.local" {
			t.Fatalf("an allowed room got no notice: %+v", s)
		}
	})
}
