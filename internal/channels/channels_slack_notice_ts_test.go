// SPDX-License-Identifier: Apache-2.0

package channels

// channels (E33), e20 N1: the Slack adapter reports the ts Slack gave the
// message it posted (MessageIDSender), so the humanoid can anchor a DM-level
// approval answer to the notice's own ts instead of the local clock.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
)

func TestChannelsSlackSendWithIDReturnsPostedTS(t *testing.T) {
	const bot, app = "xoxb-channels-n1-synthetic", "xapp-channels-n1-synthetic"
	fake := fakeplatform.NewSlack(bot, app)
	defer fake.Close()

	cfg := channelsSlackCfg(fake.URL())
	cfg.BotToken, cfg.AppToken = bot, app
	a := NewSlackAdapter(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	var _ MessageIDSender = a

	out := OutboundMessage{RecipientID: "UOWNER0001", Content: "[approval required] n1"}
	var sent SentMessage
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for {
		sent, err = a.SendWithID(ctx, out)
		if err == nil || !strings.Contains(err.Error(), "not connected") || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("SendWithID: %v", err)
	}
	sends := fake.Sends()
	if len(sends) != 1 {
		t.Fatalf("posts = %d, want 1", len(sends))
	}
	// The fake numbers posts "<n>.000200"; the adapter must hand back exactly
	// the ts Slack answered with.
	if sent.ID == "" || !strings.HasSuffix(sent.ID, ".000200") {
		t.Fatalf("SendWithID ts = %q, want the ts chat.postMessage returned", sent.ID)
	}
	// A post to a user id is delivered in that user's DM; the adapter must
	// report the channel Slack answered with (C1).
	if sent.Channel != fake.DMChannel("UOWNER0001") {
		t.Fatalf("SendWithID channel = %q, want the DM channel chat.postMessage returned", sent.Channel)
	}
}
