// SPDX-License-Identifier: Apache-2.0

package channels

// gap05 (G13 c2): the Matrix adapter against a local fake homeserver
// (client-server API v3). v1 reads unencrypted rooms only.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const mxToken = "fake-matrix-token-gap05"

func startMatrix(t *testing.T, cc msg.ChannelConfig, before func(*fakeplatform.Matrix)) (*fakeplatform.Matrix, ChannelAdapter, context.Context) {
	t.Helper()
	fake := fakeplatform.NewMatrix(mxToken)
	t.Cleanup(fake.Close)
	if before != nil {
		before(fake)
	}
	cc.ServerURL = fake.URL()
	cc.BotToken = mxToken
	a, err := NewAdapter("matrix", cc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start against the fake homeserver: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return fake, a, ctx
}

func TestMatrixIsABuiltInChannel(t *testing.T) {
	if !IsValidChannelType("matrix") {
		t.Fatal("matrix is not a valid channel type")
	}
}

func TestMatrixRoundTripAgainstFake(t *testing.T) {
	const alice = "@alice:fake.local"
	fake, a, ctx := startMatrix(t, msg.ChannelConfig{ReplyMode: "mentions"}, func(f *fakeplatform.Matrix) {
		// History from before the bot started must not be answered.
		f.PushDM(alice, "an old message")
	})
	if !a.Status().Connected {
		t.Fatalf("status = %+v", a.Status())
	}
	noInbound(t, a)

	fake.PushDM(alice, "hello")
	dm := nextInbound(t, a)
	if dm.SenderID != alice || dm.ThreadID != fake.DMRoom(alice) || dm.Content != "hello" ||
		dm.Metadata["direct_message"] != "true" || dm.Metadata["observe_only"] == "true" {
		t.Fatalf("DM inbound = %+v", dm)
	}

	const room = "!team:fake.local"
	fake.PushRoom(room, 5, "@bob:fake.local", "lunch?")
	if im := nextInbound(t, a); im.Metadata["observe_only"] != "true" {
		t.Fatalf("un-addressed room message not observe-only: %+v", im)
	}
	fake.PushRoom(room, 5, "@bob:fake.local", "rysh: status?")
	if im := nextInbound(t, a); im.Metadata["mention"] != "true" || im.Metadata["observe_only"] == "true" {
		t.Fatalf("mention not addressed: %+v", im)
	}
	// The bot's own message is dropped (encrypted events:
	// TestMatrixEncryptedRoomIsNeverSilent).
	fake.PushRoom(room, 5, fake.UserID, "my own words")
	noInbound(t, a)

	if err := a.Send(ctx, OutboundMessage{RecipientID: dm.SenderID, ThreadID: dm.ThreadID, Content: "hi back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := a.Send(ctx, OutboundMessage{ThreadID: dm.ThreadID, Content: "thinking", Kind: OutboundKindStep}); err != nil {
		t.Fatalf("Send step: %v", err)
	}
	s := fake.Sends()
	if len(s) != 2 || s[0].RoomID != fake.DMRoom(alice) || s[0].Body != "hi back" || s[0].MsgType != "m.text" ||
		s[1].MsgType != "m.notice" || s[0].TxnID == s[1].TxnID {
		t.Fatalf("sends = %+v", s)
	}
}

func TestMatrixJoinsInvites(t *testing.T) {
	fake, a, _ := startMatrix(t, msg.ChannelConfig{}, nil)
	fake.Invite("!new:fake.local", "@alice:fake.local")
	deadlineJoin(t, fake, "!new:fake.local")
	_ = a
}

func TestMatrixRoomAllowList(t *testing.T) {
	fake, a, _ := startMatrix(t, msg.ChannelConfig{Channels: []string{"!team:fake.local"}}, nil)
	fake.PushRoom("!other:fake.local", 5, "@bob:fake.local", "hi")
	fake.PushDM("@alice:fake.local", "hi") // a DM does not bypass the allow-list
	noInbound(t, a)
	fake.PushRoom("!team:fake.local", 5, "@bob:fake.local", "hi")
	if im := nextInbound(t, a); im.ThreadID != "!team:fake.local" {
		t.Fatalf("inbound = %+v", im)
	}
	// An invite to a room outside the allow-list is not joined.
	fake.Invite("!stranger-room:fake.local", "@mallory:fake.local")
	noInbound(t, a)
	for _, j := range fake.Joins() {
		if j == "!stranger-room:fake.local" {
			t.Fatal("joined a room outside the allow-list")
		}
	}
}

func TestMatrixValidateIdentity(t *testing.T) {
	fake := fakeplatform.NewMatrix(mxToken)
	defer fake.Close()
	a, _ := NewAdapter("matrix", msg.ChannelConfig{ServerURL: fake.URL(), BotToken: mxToken})
	id, err := a.(IdentityValidator).ValidateIdentity(context.Background())
	if err != nil || id.ID != fake.UserID || id.Username != "rysh" {
		t.Fatalf("identity = %+v err=%v", id, err)
	}
	bad, _ := NewAdapter("matrix", msg.ChannelConfig{ServerURL: fake.URL(), BotToken: "wrong-token"})
	if _, err := bad.(IdentityValidator).ValidateIdentity(context.Background()); err == nil || strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("a rejected token: err=%v", err)
	}
}

// Run under sandbox-exec deny-network: the refusal comes before any request.
func TestMatrixRefusesCleartextRemoteHomeserver(t *testing.T) {
	for _, cc := range []msg.ChannelConfig{
		{ServerURL: "http://matrix.example.org", BotToken: mxToken},
		{BotToken: mxToken},
		{ServerURL: "https://matrix.example.org"},
	} {
		a, _ := NewAdapter("matrix", cc)
		err := a.Start(context.Background())
		if err == nil {
			_ = a.Stop()
			t.Fatalf("Start accepted %+v", cc.ServerURL)
		}
		if strings.Contains(err.Error(), mxToken) {
			t.Fatal("refusal leaked the token")
		}
		// The refusal must come from the config check, not from a request
		// that failed: name the field at fault.
		if want := map[bool]string{true: "server_url", false: "bot_token"}[cc.BotToken != ""]; !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal for %q does not name %s: %v", cc.ServerURL, want, err)
		}
	}
}

func deadlineJoin(t *testing.T, fake *fakeplatform.Matrix, room string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range fake.Joins() {
			if j == room {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the bot never joined %s (joins=%v)", room, fake.Joins())
}
