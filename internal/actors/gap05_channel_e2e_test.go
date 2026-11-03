// SPDX-License-Identifier: Apache-2.0

package actors

// gap05 (G13): channels end to end against a local fake platform.
//
// The real channel adapter is started by the humanoid's own startChannel,
// pointed at a fake platform server on 127.0.0.1 (internal/channels/
// fakeplatform). Inbound traffic goes fake → adapter → inboundLoop → NATS
// humanoid inbox; the test stands in for the actor mailbox only, handing each
// published MsgHumanoidInboundMessage to handleInboundMessage. Every outbound
// call is read back from the fake, so each assertion is about what would have
// reached the platform. This proves rysh's side of the wire; it is not a live
// proof.
//
// One scenario, run per platform:
//  1. A stranger's DM raises a pairing request; nothing reaches the LLM and
//     nothing is posted.
//  2. The approver approves the code; the sender's next DM is addressed (the
//     DM rule, under reply_mode: mentions) and reaches the LLM.
//  3. Under governance: human the reply is held as a draft: nothing posted
//     until "send", which posts exactly once to the sender's chat; "discard"
//     posts nothing.
//  4. A tool approval raised while serving the owner is routed to the owner's
//     chat; a stranger's "yes" releases nothing; the owner's "yes" decides it.
//  5. A group message without a mention is observe-only: no LLM, no post.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// e2ePlatform adapts one fake platform to the shared scenario.
type e2ePlatform struct {
	channelType string
	cc          msg.ChannelConfig
	owner       string
	stranger    string
	intruder    string
	// chatOf is the chat a reply to user's DM must be posted to.
	chatOf func(user string) string
	// dm sends a 1:1 message from user id to the bot.
	dm func(id, text string)
	// group posts an un-addressed message in a group/guild channel; nil on a
	// platform with no group chats (WhatsApp Cloud API), which skips step 5.
	group func(id, text string)
	// sends returns (chat id, text) for every message the fake received.
	sends func() [][2]string
	close func()
	// afterStart, when set, sees the started adapter (LINE needs its local
	// webhook URL to deliver to).
	afterStart func(channels.ChannelAdapter)
	// approvalReply, when set, is how a user answers an approval notice in
	// step 4; nil means a plain dm. Slack threads every top-level message
	// (ThreadID = its ts), and approvals match by thread, so a Slack user
	// answers inside the notice's thread.
	approvalReply func(id, text string)
	// noDMTag: the platform has no 1:1-vs-group distinction (email), so its
	// adapter never tags direct_message; step 2 then checks only that the
	// approved sender's message is served, not observe-only.
	noDMTag bool
	// approvalPinned, when set, names a pinned finding: on this platform the
	// owner's answer does NOT resolve the approval today (it fails closed).
	// Step 4 then asserts exactly that, so a change to the rule shows up
	// here, and moves on. Changing it is a gate change for security-lead.
	approvalPinned string
	// emailFake is the email platform's fake, for variants of its scenario.
	emailFake *fakeplatform.Email
	// prepare, when set, sees the humanoid before startChannel (email hands
	// it a pre-built adapter wired to the fake IMAP server).
	prepare func(h *HumanoidActor)
}

func telegramE2EPlatform(t *testing.T) *e2ePlatform {
	const token = "fake-token-gap05"
	fake := fakeplatform.NewTelegram(token)
	id := func(s string) int64 {
		var n int64
		for _, c := range s {
			n = n*10 + int64(c-'0')
		}
		return n
	}
	return &e2ePlatform{
		channelType: "telegram",
		cc: msg.ChannelConfig{
			Enabled:       true,
			BotToken:      token,
			APIBase:       fake.URL(),
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{"5550001"},
		},
		owner:    "5550001",
		stranger: "5550099",
		intruder: "5550777",
		chatOf:   func(user string) string { return user },
		dm:       func(from, text string) { fake.PushPrivate(id(from), "u"+from, text) },
		group:    func(from, text string) { fake.PushGroup(-100200300, id(from), "u"+from, text) },
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.ChatID, s.Text})
			}
			return out
		},
		close: fake.Close,
	}
}

func discordE2EPlatform(t *testing.T) *e2ePlatform {
	const token = "fake-discord-token-gap05"
	fake := fakeplatform.NewDiscord(token)
	const owner = "100000000000000001"
	return &e2ePlatform{
		channelType: "discord",
		cc: msg.ChannelConfig{
			Enabled:       true,
			BotToken:      token,
			APIBase:       fake.URL(),
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{owner},
		},
		owner:    owner,
		stranger: "100000000000000099",
		intruder: "100000000000000777",
		chatOf:   fake.DMChannel,
		dm:       func(from, text string) { fake.PushDM(from, "u"+from[len(from)-3:], text) },
		group: func(from, text string) {
			fake.PushGuild("800000000000000001", "800000000000000002", from, "u"+from[len(from)-3:], text)
		},
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.ChannelID, s.Content})
			}
			return out
		},
		close: fake.Close,
	}
}

func mattermostE2EPlatform(t *testing.T) *e2ePlatform {
	const token = "fake-mattermost-token-gap05"
	fake := fakeplatform.NewMattermost(token)
	return &e2ePlatform{
		channelType: "mattermost",
		cc: msg.ChannelConfig{
			Enabled:       true,
			BotToken:      token,
			ServerURL:     fake.URL(),
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{"userowner"},
		},
		owner:    "userowner",
		stranger: "userstranger",
		intruder: "userintruder",
		chatOf:   fake.DMChannel,
		dm:       func(from, text string) { fake.PushDM(from, from, text) },
		group:    func(from, text string) { fake.PushChannel("townsquare", from, from, text) },
		sends: func() [][2]string {
			var out [][2]string
			for _, p := range fake.Posts() {
				out = append(out, [2]string{p.ChannelID, p.Message})
			}
			return out
		},
		close: fake.Close,
	}
}

func TestGap05MattermostEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, mattermostE2EPlatform(t))
}

func matrixE2EPlatform(t *testing.T) *e2ePlatform {
	const token = "fake-matrix-token-gap05"
	fake := fakeplatform.NewMatrix(token)
	return &e2ePlatform{
		channelType: "matrix",
		cc: msg.ChannelConfig{
			Enabled:       true,
			BotToken:      token,
			ServerURL:     fake.URL(),
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{"@owner:fake.local"},
		},
		owner:    "@owner:fake.local",
		stranger: "@stranger:fake.local",
		intruder: "@intruder:fake.local",
		chatOf:   fake.DMRoom,
		dm:       func(from, text string) { fake.PushDM(from, text) },
		group:    func(from, text string) { fake.PushRoom("!team:fake.local", 5, from, text) },
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.RoomID, s.Body})
			}
			return out
		},
		close: fake.Close,
	}
}

func TestGap05MatrixEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, matrixE2EPlatform(t))
}

func lineE2EPlatform(t *testing.T) *e2ePlatform {
	const token, secret = "fake-line-token-gap05", "fake-line-secret-gap05"
	fake := fakeplatform.NewLine(token, secret)
	var hook string
	return &e2ePlatform{
		channelType: "line",
		cc: msg.ChannelConfig{
			Enabled:       true,
			BotToken:      token,
			AppSecret:     secret,
			APIBase:       fake.URL(),
			WebhookPort:   "0",
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{"Uowner000000000000000000000000001"},
		},
		owner:    "Uowner000000000000000000000000001",
		stranger: "Ustranger0000000000000000000000001",
		intruder: "Uintruder0000000000000000000000001",
		chatOf:   func(user string) string { return user },
		dm:       func(from, text string) { fake.DeliverDM(hook, from, text) },
		group: func(from, text string) {
			fake.DeliverGroup(hook, "Cgroup00000000000000000000000001", from, text, false)
		},
		sends: func() [][2]string {
			var out [][2]string
			for _, p := range fake.Pushes() {
				out = append(out, [2]string{p.To, p.Text})
			}
			return out
		},
		close: fake.Close,
		afterStart: func(a channels.ChannelAdapter) {
			hook = a.(interface{ LocalWebhookURL() string }).LocalWebhookURL()
		},
	}
}

func TestGap05LineEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, lineE2EPlatform(t))
}

func TestGap05TelegramEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, telegramE2EPlatform(t))
}

func TestGap05DiscordEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, discordE2EPlatform(t))
}

// runChannelE2E is the shared scenario (see the file comment).
func runChannelE2E(t *testing.T, p *e2ePlatform) {
	t.Helper()
	defer p.close()
	t.Chdir(t.TempDir())
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	const name = "e2e-bot"
	h := &HumanoidActor{
		name:               name,
		profile:            "assistant",
		active:             true,
		pub:                pub,
		nc:                 nc,
		llmPromptExecInbox: msg.T("pane", name, "llm_prompt_execution", "inbox"),
		conversations:      make(map[string]*ConversationContext),
		contacts:           map[string]msg.ChannelConfig{p.channelType: p.cc},
		governance:         map[string]string{p.channelType: p.cc.Governance},
		adapters:           map[string]channels.ChannelAdapter{},
		adapterCtxs:        map[string]context.CancelFunc{},
		pairing:            channels.NewPairingStore(nil), // file fallback
	}
	h.mergeDeclaredAllowlists() // Started does this at spawn
	if p.channelType == "whatsapp" {
		// WhatsApp keeps its own field (NewHumanoidActor reads it from cc).
		h.whatsappGovernance = p.cc.Governance
	}

	sub := func(subject string) *nats.Subscription {
		s, err := nc.SubscribeSync(subject)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	inbox := sub(msg.T("humanoid", name, "inbox"))
	pairingSub := sub(msg.T("humanoid", name, "pairing"))
	llmSub := sub(h.llmPromptExecInbox)
	respSub := sub(msg.T("pane", name, "approval", "response"))

	if p.prepare != nil {
		p.prepare(h)
	}
	h.startChannel(p.channelType, p.cc)
	if _, ok := h.adapters[p.channelType]; !ok {
		t.Fatalf("%s adapter did not start against the fake platform", p.channelType)
	}
	defer h.stopChannel(p.channelType)
	if p.afterStart != nil {
		p.afterStart(h.adapters[p.channelType])
	}

	// deliver waits for the adapter to publish the next inbound and hands it
	// to the humanoid, as the actor mailbox would.
	deliver := func() *msg.MsgHumanoidInboundMessage {
		t.Helper()
		m, err := inbox.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("no inbound reached the humanoid inbox: %v", err)
		}
		var env natsEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Fatal(err)
		}
		var in msg.MsgHumanoidInboundMessage
		if err := json.Unmarshal(env.Payload, &in); err != nil {
			t.Fatal(err)
		}
		h.handleInboundMessage(&in)
		return &in
	}
	// quietSends is for "nothing more was posted": a short settle, then the
	// count.
	quietSends := func() [][2]string {
		time.Sleep(300 * time.Millisecond)
		return p.sends()
	}
	waitSends := func(n int) [][2]string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got := p.sends()
			if len(got) >= n || time.Now().After(deadline) {
				// Give a stray extra post a moment to show up.
				time.Sleep(150 * time.Millisecond)
				return p.sends()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// 1. A stranger's DM → pairing request, no LLM, no post.
	p.dm(p.stranger, "let me in")
	deliver()
	envs := drainSubject(t, pairingSub, 500*time.Millisecond)
	if len(envs) != 1 || envs[0].TypeTag != msg.TagChannelPairRequest {
		t.Fatalf("want one pairing request for the stranger, got %d", len(envs))
	}
	var req msg.MsgChannelPairRequest
	if err := json.Unmarshal(envs[0].Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.SenderID != p.stranger || req.Channel != p.channelType {
		t.Fatalf("pairing request = %+v", req)
	}
	if got := drainSubject(t, llmSub, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("a stranger's DM reached the LLM (%d)", len(got))
	}
	if got := quietSends(); len(got) != 0 {
		t.Fatalf("something was posted to the platform for a stranger: %v", got)
	}

	// 2. Approve → the next DM is addressed and served.
	h.handlePairApprove(&msg.MsgChannelPairApprove{HumanoidName: name, Code: req.Code})
	p.dm(p.stranger, "what's the eta?")
	in := deliver()
	if in.Metadata["observe_only"] == "true" || (!p.noDMTag && in.Metadata["direct_message"] != "true") {
		t.Fatalf("an approved DM was not addressed: metadata=%v", in.Metadata)
	}
	if h.lastInbound == nil || h.lastInbound.SenderID != p.stranger {
		t.Fatal("the approved sender's DM was not served")
	}
	if got := drainSubject(t, llmSub, 500*time.Millisecond); len(got) == 0 {
		t.Fatal("the approved sender's DM produced no LLM prompt")
	}

	// 3. Human governance: held as a draft until "send". A tool-governed
	// channel (WhatsApp: whatsapp_draft/whatsapp_send) runs this scenario under
	// governance: ai instead, where the reply posts straight back, once.
	if p.cc.Governance != "human" {
		replyCompletes(h, "ETA is Friday.")
		got := waitSends(1)
		if len(got) != 1 || got[0][0] != p.chatOf(p.stranger) || got[0][1] != "ETA is Friday." {
			t.Fatalf("governance %s: platform got %v; want exactly one post to %s", p.cc.Governance, got, p.chatOf(p.stranger))
		}
	} else {
		runDraftRound(t, h, p, deliver, quietSends, waitSends)
	}
	runOwnerApprovalAndObserve(t, h, p, name, deliver, quietSends, waitSends, llmSub, respSub)
}

// runDraftRound is step 3 under governance: human.
func runDraftRound(t *testing.T, h *HumanoidActor, p *e2ePlatform, deliver func() *msg.MsgHumanoidInboundMessage,
	quietSends func() [][2]string, waitSends func(int) [][2]string) {
	t.Helper()
	replyCompletes(h, "ETA is Friday.")
	if len(h.pendingGenericDrafts) != 1 {
		t.Fatalf("reply not held as a draft (queue=%d)", len(h.pendingGenericDrafts))
	}
	if got := quietSends(); len(got) != 0 {
		t.Fatalf("a draft reached the platform before send: %v", got)
	}
	if !h.handleGenericDraftCommand("send") {
		t.Fatal(`"send" was not taken as a draft command`)
	}
	got := waitSends(1)
	if len(got) != 1 || got[0][0] != p.chatOf(p.stranger) || got[0][1] != "ETA is Friday." {
		t.Fatalf("after send, platform got %v; want exactly one post to %s", got, p.chatOf(p.stranger))
	}

	p.dm(p.stranger, "and the price?")
	deliver()
	replyCompletes(h, "It is 10 pounds.")
	if !h.handleGenericDraftCommand("discard") {
		t.Fatal(`"discard" was not taken as a draft command`)
	}
	if got := quietSends(); len(got) != 1 {
		t.Fatalf("discard posted something: %v", got)
	}
}

// runOwnerApprovalAndObserve is steps 4 and 5; one post (step 3) precedes it.
func runOwnerApprovalAndObserve(t *testing.T, h *HumanoidActor, p *e2ePlatform, name string,
	deliver func() *msg.MsgHumanoidInboundMessage, quietSends func() [][2]string, waitSends func(int) [][2]string,
	llmSub, respSub *nats.Subscription) {
	t.Helper()

	// 4. A tool approval while serving the owner.
	p.dm(p.owner, "deploy the fix")
	deliver()
	if h.lastInbound == nil || h.lastInbound.SenderID != p.owner {
		t.Fatal("the owner's DM was not served")
	}
	_ = drainSubject(t, llmSub, 200*time.Millisecond)
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-e2e", Description: "bash: make deploy"})
	got := waitSends(2)
	if len(got) != 2 || got[1][0] != p.chatOf(p.owner) || !strings.Contains(got[1][1], "approval required") {
		t.Fatalf("approval notice not posted to the owner's chat: %v", got)
	}
	answer := p.dm
	if p.approvalReply != nil {
		answer = p.approvalReply
	}
	answer(p.intruder, "yes")
	deliver()
	if _, ok := h.pendingApprovals["req-e2e"]; !ok {
		t.Fatal("an intruder's yes cleared the approval")
	}
	if got := drainSubject(t, respSub, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("an intruder's yes released the approval (%d)", len(got))
	}
	answer(p.owner, "yes")
	deliver()
	resp := drainSubject(t, respSub, time.Second)
	if p.approvalPinned != "" {
		if len(resp) != 0 {
			t.Fatalf("pinned finding changed (%s): the owner's answer now resolves the approval; update the pin with security-lead", p.approvalPinned)
		}
		if _, ok := h.pendingApprovals["req-e2e"]; !ok {
			t.Fatalf("pinned finding changed (%s): the approval is no longer pending", p.approvalPinned)
		}
		t.Logf("%s: pinned finding, approval stays pending (fails closed): %s", p.channelType, p.approvalPinned)
		delete(h.pendingApprovals, "req-e2e")
	} else if len(resp) != 1 {
		t.Fatalf("want one approval response from the owner's yes, got %d", len(resp))
	}
	if p.approvalPinned == "" {
		var decision msg.MsgApprovalResponse
		if err := json.Unmarshal(resp[0].Payload, &decision); err != nil {
			t.Fatal(err)
		}
		if decision.RequestID != "req-e2e" || decision.Decision != msg.DecisionYes {
			t.Fatalf("approval response = %+v", decision)
		}
	}
	replyCompletes(h, "")

	// 5. An un-addressed group message is observe-only.
	if p.group == nil {
		t.Logf("%s has no group chats: step 5 (observe-only) does not apply", p.channelType)
		return
	}
	before := len(p.sends())
	p.group(p.owner, "lunch anyone?")
	in := deliver()
	if in.Metadata["observe_only"] != "true" {
		t.Fatalf("an un-addressed group message was not observe-only: %v", in.Metadata)
	}
	if got := drainSubject(t, llmSub, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("an observe-only message reached the LLM (%d)", len(got))
	}
	if got := quietSends(); len(got) != before {
		t.Fatalf("an observe-only message caused a post: %v", got[before:])
	}
}
