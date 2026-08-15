// SPDX-License-Identifier: Apache-2.0

package actors

// Tests for the assistant profile's fail-closed safety inversion (design 007
// PM3) and the per-humanoid provider selection (design 006 MP2). They use the
// same spawn-less harness pattern as humanoid_pairing_test.go: handlers are
// driven directly on a HumanoidActor struct over an in-process NATS conn.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// TestHumanoidAutoApproveDefaultsTrue locks the decision seam the Started
// handler feeds into SetAutoApproveAll. The rule is the skill file's
// `auto_approve:` field, defaulting to TRUE for every humanoid — profile and
// name are NOT inputs.
//
// This replaces the earlier PM3 rule where `profile: assistant` alone forced
// gating. That coupling failed in practice: every humanoid, assistant
// included, is driven by a chat channel with nobody at the keyboard, so the
// approval it raised waited in a pane no one was watching and read as the
// humanoid being broken. Gating is now an explicit, independent opt-in.
func TestHumanoidAutoApproveDefaultsTrue(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name, profile string
		autoApprove   *bool
		want          bool
	}{
		// Absent field → the default, regardless of profile or name.
		{"support", "", nil, true},
		{"support", "assistant", nil, true},
		{"assistant", "", nil, true},
		{"assistant", "assistant", nil, true},
		// Explicit false gates — and it works for ANY humanoid, not just the
		// assistant profile. That generality is the point of the change.
		{"assistant", "assistant", &no, false},
		{"support", "", &no, false},
		// Explicit true is honoured as written.
		{"assistant", "assistant", &yes, true},
	}
	for _, c := range cases {
		h := &HumanoidActor{name: c.name, profile: c.profile, autoApprove: c.autoApprove}
		if got := h.autoApproveTools(); got != c.want {
			t.Errorf("autoApproveTools(name=%q profile=%q auto_approve=%v) = %v, want %v",
				c.name, c.profile, c.autoApprove, got, c.want)
		}
	}
}

// captureAdapter records outbound sends so tests can assert the approval
// draft is routed back over the originating channel.
type captureAdapter struct {
	inbound chan channels.InboundMessage
	sent    []channels.OutboundMessage
}

func (c *captureAdapter) Type() string                { return "telegram" }
func (c *captureAdapter) Start(context.Context) error { return nil }
func (c *captureAdapter) Stop() error                 { return nil }
func (c *captureAdapter) Send(_ context.Context, m channels.OutboundMessage) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureAdapter) InboundCh() <-chan channels.InboundMessage { return c.inbound }
func (c *captureAdapter) Status() msg.ChannelStatus                 { return msg.ChannelStatus{Type: "telegram"} }
func (c *captureAdapter) SetReplyMode(string)                       {}

var _ channels.ChannelAdapter = (*captureAdapter)(nil)

// newAssistantForTest builds a minimal assistant-profile HumanoidActor over an
// in-process NATS publisher, gated owner-only on telegram.
func newAssistantForTest(t *testing.T, owner string) (*HumanoidActor, *captureAdapter, func(window time.Duration) []natsEnvelope) {
	t.Helper()
	t.Chdir(t.TempDir())
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	adapter := &captureAdapter{inbound: make(chan channels.InboundMessage)}
	h := &HumanoidActor{
		name:               "assistant",
		profile:            "assistant",
		active:             true,
		pub:                pub,
		nc:                 nc,
		llmPromptExecInbox: msg.T("pane", "assistant", "llm_prompt_execution", "inbox"),
		conversations:      make(map[string]*ConversationContext),
		contacts: map[string]msg.ChannelConfig{
			"telegram": {
				Enabled:       true,
				Governance:    "human",
				ReplyMode:     "messages",
				PairingPolicy: "request",
				Allowlist:     []string{owner},
			},
		},
		adapters: map[string]channels.ChannelAdapter{"telegram": adapter},
		pairing:  channels.NewPairingStore(nil), // file fallback
	}
	h.mergeDeclaredAllowlists() // Started does this at spawn

	respSub, err := nc.SubscribeSync(msg.T("pane", "assistant", "approval", "response"))
	if err != nil {
		t.Fatalf("subscribe approval response: %v", err)
	}
	drain := func(window time.Duration) []natsEnvelope { return drainSubject(t, respSub, window) }
	return h, adapter, drain
}

func assistantInbound(sender, content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram",
		SenderID:    sender,
		SenderName:  sender,
		ThreadID:    "t1",
		Content:     content,
	}
}

// TestAssistantApprovalHeldRoutedAndOwnerConfirmed is the PM3 proof: a tool
// approval on the assistant is HELD (no auto-approve response), surfaced as a
// draft over the originating channel, cannot be released by a non-allowlisted
// sender, and is released only by the owner's "yes".
func TestAssistantApprovalHeldRoutedAndOwnerConfirmed(t *testing.T) {
	h, adapter, drainResp := newAssistantForTest(t, "owner-1")
	h.lastInbound = assistantInbound("owner-1", "book the meeting")

	h.handleApprovalRequest(&msg.MsgApprovalRequest{
		RequestID: "req-1", Description: "bash: curl -X POST https://calendar…",
	})

	// Held, not auto-approved.
	if _, ok := h.pendingApprovals["req-1"]; !ok {
		t.Fatalf("approval not held pending: %+v", h.pendingApprovals)
	}
	if got := drainResp(300 * time.Millisecond); len(got) != 0 {
		t.Fatalf("assistant must not auto-approve; published %+v", got)
	}
	// Routed to the owner over the same channel as a draft notice.
	if len(adapter.sent) != 1 || !strings.Contains(adapter.sent[0].Content, "approval required") {
		t.Fatalf("approval draft not routed over channel: %+v", adapter.sent)
	}
	if adapter.sent[0].RecipientID != "owner-1" {
		t.Fatalf("draft routed to %q, want owner-1", adapter.sent[0].RecipientID)
	}

	// A stranger cannot release it: the admission gate holds them (owner-only
	// allowlist), the pending approval survives, and no response is published.
	h.handleInboundMessage(assistantInbound("intruder", "yes"))
	if _, ok := h.pendingApprovals["req-1"]; !ok {
		t.Fatal("stranger cleared the pending approval")
	}
	if got := drainResp(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("stranger released the approval: %+v", got)
	}

	// The owner's channel reply is the decision.
	h.handleInboundMessage(assistantInbound("owner-1", "yes"))
	envs := drainResp(time.Second)
	if len(envs) != 1 {
		t.Fatalf("want exactly one approval response, got %+v", envs)
	}
	var resp msg.MsgApprovalResponse
	if err := json.Unmarshal(envs[0].Payload, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RequestID != "req-1" || resp.Decision != msg.DecisionYes {
		t.Fatalf("response = %+v, want req-1 approved", resp)
	}
	if len(h.pendingApprovals) != 0 {
		t.Fatal("pending approval not cleared after owner decision")
	}
}

// TestAssistantApprovalOwnerRejects: any non-yes owner text rejects (with the
// text as the reason) — worst case stays a draft.
func TestAssistantApprovalOwnerRejects(t *testing.T) {
	h, _, drainResp := newAssistantForTest(t, "owner-1")
	h.lastInbound = assistantInbound("owner-1", "email my landlord")
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-2", Description: "email_send"})

	h.handleInboundMessage(assistantInbound("owner-1", "no, use the other draft"))
	envs := drainResp(time.Second)
	if len(envs) != 1 {
		t.Fatalf("want one approval response, got %+v", envs)
	}
	var resp msg.MsgApprovalResponse
	_ = json.Unmarshal(envs[0].Payload, &resp)
	if resp.Decision != msg.DecisionNoWithExplanation || resp.RequestID != "req-2" {
		t.Fatalf("response = %+v, want rejection with explanation", resp)
	}
}

// TestTeamHumanoidStillAutoApprovesStrayRequest: the pre-WS7 behaviour is
// unchanged for non-assistant humanoids — a stray approval request is approved
// immediately (Problem 2).
func TestTeamHumanoidStillAutoApprovesStrayRequest(t *testing.T) {
	t.Chdir(t.TempDir())
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	h := &HumanoidActor{
		name:               "support",
		active:             true,
		pub:                pub,
		nc:                 nc,
		llmPromptExecInbox: msg.T("pane", "support", "llm_prompt_execution", "inbox"),
		conversations:      make(map[string]*ConversationContext),
		contacts:           map[string]msg.ChannelConfig{"slack": {Enabled: true}},
	}
	respSub, err := nc.SubscribeSync(msg.T("pane", "support", "approval", "response"))
	if err != nil {
		t.Fatal(err)
	}

	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-9", Description: "bash: ls"})

	if len(h.pendingApprovals) != 0 {
		t.Fatal("team humanoid buffered an approval; want immediate auto-approve")
	}
	envs := drainSubject(t, respSub, time.Second)
	if len(envs) != 1 {
		t.Fatalf("want one auto-approve response, got %+v", envs)
	}
	var resp msg.MsgApprovalResponse
	_ = json.Unmarshal(envs[0].Payload, &resp)
	if resp.RequestID != "req-9" || resp.Decision != msg.DecisionYes {
		t.Fatalf("response = %+v, want req-9 auto-approved", resp)
	}
}

// ---------------------------------------------------------------------------
// MP2 — per-humanoid provider selection
// ---------------------------------------------------------------------------

// TestParseHumanoidFileProviderAndProfile: the frontmatter `provider:` and
// `profile:` fields parse into the definition, with ${ENV} expansion; a file
// without them yields empty fields (today's behaviour, untouched).
func TestParseHumanoidFileProviderAndProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	content := `---
name: helper
description: provider-selected humanoid
provider: ${HELPER_PROVIDER}
model: llama3.1
profile: assistant
contacts:
  telegram:
    bot_token: "${TG_TOKEN}"
    allowlist: ["123"]
---
You are helper.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELPER_PROVIDER", "ollama")

	def, err := parseHumanoidFile(path, nil)
	if err != nil {
		t.Fatalf("parseHumanoidFile: %v", err)
	}
	if def.Provider != "ollama" {
		t.Errorf("Provider = %q, want ollama (via ${ENV} expansion)", def.Provider)
	}
	if def.Profile != "assistant" || def.Model != "llama3.1" || def.Name != "helper" {
		t.Errorf("definition = %+v", def)
	}
	if got := def.Contacts["telegram"].Allowlist; len(got) != 1 || got[0] != "123" {
		t.Errorf("allowlist = %v", got)
	}

	// No provider/profile keys → empty fields (backward compatible).
	plain := filepath.Join(dir, "PLAIN.md")
	if err := os.WriteFile(plain, []byte("---\nname: plain\nmodel: m\n---\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdef, err := parseHumanoidFile(plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pdef.Provider != "" || pdef.Profile != "" {
		t.Errorf("plain file gained provider/profile: %+v", pdef)
	}
}

// TestProviderFamily locks the alias normalization the override gate uses.
func TestProviderFamily(t *testing.T) {
	for in, want := range map[string]string{
		"": "anthropic", "claude": "anthropic", "Claude-Agentic": "anthropic",
		"anthropic": "anthropic", "OpenAI": "openai", "ollama": "ollama",
		" Ollama ": "ollama",
	} {
		if got := providerFamily(in); got != want {
			t.Errorf("providerFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSetupForProviderOverride: an empty or family-matching frontmatter
// provider returns the shared Setup unchanged (exactly today's behaviour); a
// differing one yields a shallow copy running on the selected provider while
// sharing everything else — and never mutates the shared Setup.
func TestSetupForProviderOverride(t *testing.T) {
	base := &agentic.Setup{Provider: provider.NewAgenticProvider(config.Config{
		ProviderName: "claude", APIKey: "sk-test",
	})}
	cfg := config.Config{ProviderName: "claude", APIKey: "sk-test", DefaultModel: "claude-sonnet-5"}

	for _, sel := range []string{"", "claude", "anthropic", "claude-agentic"} {
		h := &HumanoidActor{name: "x", cfg: cfg, agSetup: base, provider: sel}
		if got := h.setupForProvider(); got != base {
			t.Errorf("provider %q must reuse the shared Setup, got a copy", sel)
		}
	}

	h := &HumanoidActor{name: "x", cfg: cfg, agSetup: base, provider: "ollama"}
	got := h.setupForProvider()
	if got == base {
		t.Fatal("ollama override returned the shared Setup")
	}
	if got.Provider == nil || got.Provider.Name() != "ollama" {
		t.Fatalf("override Provider = %v, want ollama", got.Provider)
	}
	if base.Provider.Name() == "ollama" {
		t.Fatal("shared Setup was mutated by the override")
	}
	// Shallow copy: everything except Provider is shared.
	if got.ToolRegistry != base.ToolRegistry || got.SecretNAT != base.SecretNAT {
		t.Error("override Setup must share the base's registries/managers")
	}
}

// TestHumanoidAutoApproveIsIndependentOfGovernance keeps the two knobs
// separate. `governance` decides whether REPLIES post without a release step;
// `auto_approve` decides whether TOOL CALLS run without an approval. Collapsing
// them would mean changing one silently changed the other.
func TestHumanoidAutoApproveIsIndependentOfGovernance(t *testing.T) {
	no := false
	for _, gov := range []string{"ai", "human", ""} {
		gated := &HumanoidActor{
			name: "assistant", profile: "assistant", autoApprove: &no,
			contacts: map[string]msg.ChannelConfig{"slack": {Governance: gov}},
		}
		if gated.autoApproveTools() {
			t.Errorf("governance %q: auto_approve:false must gate tools whatever governance says", gov)
		}
		def := &HumanoidActor{
			name: "assistant", profile: "assistant",
			contacts: map[string]msg.ChannelConfig{"slack": {Governance: gov}},
		}
		if !def.autoApproveTools() {
			t.Errorf("governance %q: the default must stay true whatever governance says", gov)
		}
	}
}

// TestAgentAutoApproveDefaultsTrue covers the gap this flag was introduced to
// close: agent.go never called SetAutoApproveAll at all, so every autonomous
// agent ran with gating ON and raised approvals into a pane an agent does not
// have. Nothing was watching, so the run just stopped.
func TestAgentAutoApproveDefaultsTrue(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		autoApprove *bool
		want        bool
	}{
		{nil, true},  // absent → default
		{&yes, true}, // explicit true
		{&no, false}, // explicit opt-in to gating
	} {
		a := &AgentActor{autoApprove: c.autoApprove}
		if got := a.autoApproveTools(); got != c.want {
			t.Errorf("agent autoApproveTools(%v) = %v, want %v", c.autoApprove, got, c.want)
		}
	}
}

// TestSkillFileAutoApproveTriState pins the parse contract for BOTH skill-file
// kinds. A plain bool would collapse "absent" into "false" and silently gate
// every existing humanoid and agent on upgrade — the field must stay a pointer
// so an unset file keeps the default.
func TestSkillFileAutoApproveTriState(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	id := func(s string) string { return s }

	// --- humanoid ---
	absent := write("h-absent.md", "---\nname: h\ndescription: d\ncontacts: {}\n---\nbody\n")
	hd, err := parseHumanoidFile(absent, id)
	if err != nil {
		t.Fatal(err)
	}
	if hd.AutoApprove != nil {
		t.Errorf("absent auto_approve parsed as %v, want nil (so the default applies)", *hd.AutoApprove)
	}
	gated := write("h-false.md", "---\nname: h\ndescription: d\nauto_approve: false\ncontacts: {}\n---\nbody\n")
	hd, err = parseHumanoidFile(gated, id)
	if err != nil {
		t.Fatal(err)
	}
	if hd.AutoApprove == nil || *hd.AutoApprove {
		t.Errorf("auto_approve: false parsed as %v, want explicit false", hd.AutoApprove)
	}

	// --- agent ---
	aAbsent := write("a-absent.md", "---\nname: a\ndescription: d\n---\nbody\n")
	ad, err := parseSkillFile(aAbsent, id)
	if err != nil {
		t.Fatal(err)
	}
	if ad.AutoApprove != nil {
		t.Errorf("agent absent auto_approve parsed as %v, want nil", *ad.AutoApprove)
	}
	aGated := write("a-false.md", "---\nname: a\ndescription: d\nauto_approve: false\n---\nbody\n")
	ad, err = parseSkillFile(aGated, id)
	if err != nil {
		t.Fatal(err)
	}
	if ad.AutoApprove == nil || *ad.AutoApprove {
		t.Errorf("agent auto_approve: false parsed as %v, want explicit false", ad.AutoApprove)
	}
}
