// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Gap 6 (design 019): the email governance key.
//
// `rysh skill scaffold` writes `governance:` at the channel-block top level
// for EVERY channel, but the actor used to read only the nested
// `config.governance` for email — so a scaffolded `governance: human` email
// humanoid silently initialised to ai, and the send gate never engaged.
// ---------------------------------------------------------------------------

func TestEmailGovernanceReadsTopLevelKey(t *testing.T) {
	h := NewHumanoidActor("mail-bot", "sp", map[string]msg.ChannelConfig{
		"email": {Governance: "human"}, // what the scaffold writes
	}, config.Config{}, nil, nil, nil, nil)
	if got := h.govMode("email"); got != "human" {
		t.Fatalf("scaffold-written top-level governance ignored: govMode(email) = %q, want human", got)
	}
}

func TestEmailGovernanceNestedKeyStillWorks(t *testing.T) {
	h := NewHumanoidActor("mail-bot", "sp", map[string]msg.ChannelConfig{
		"email": {EmailConfig: &msg.EmailChannelConfig{Governance: "human"}},
	}, config.Config{}, nil, nil, nil, nil)
	if got := h.govMode("email"); got != "human" {
		t.Fatalf("legacy nested config.governance broken: govMode(email) = %q, want human", got)
	}
}

// TestEmailGovernanceRestrictiveWins: when the two spellings disagree, "human"
// wins — a governance key must never be silently downgraded.
func TestEmailGovernanceRestrictiveWins(t *testing.T) {
	cases := []msg.ChannelConfig{
		{Governance: "human", EmailConfig: &msg.EmailChannelConfig{Governance: "ai"}},
		{Governance: "ai", EmailConfig: &msg.EmailChannelConfig{Governance: "human"}},
	}
	for i, cc := range cases {
		h := NewHumanoidActor("mail-bot", "sp",
			map[string]msg.ChannelConfig{"email": cc}, config.Config{}, nil, nil, nil, nil)
		if got := h.govMode("email"); got != "human" {
			t.Errorf("case %d: govMode(email) = %q, want human (restrictive wins)", i, got)
		}
	}
}

func TestEmailGovernanceDefaultsToAI(t *testing.T) {
	h := NewHumanoidActor("mail-bot", "sp", map[string]msg.ChannelConfig{
		"email": {},
	}, config.Config{}, nil, nil, nil, nil)
	if got := h.govMode("email"); got != "ai" {
		t.Fatalf("govMode(email) = %q, want the ai default", got)
	}
}

// ---------------------------------------------------------------------------
// The per-call gate closures handed to the channel send tools.
// ---------------------------------------------------------------------------

// TestChannelTools_AllThreeGatesWiredAndLive extends the Slack-only gate
// wiring tests: email and whatsapp gates must be wired the same way and must
// observe a runtime governance flip without a respawn.
func TestChannelTools_AllThreeGatesWiredAndLive(t *testing.T) {
	h := NewHumanoidActor("multi", "sp", map[string]msg.ChannelConfig{
		"slack":    {},
		"email":    {},
		"whatsapp": {},
	}, config.Config{}, nil, nil, nil, nil)

	ct := h.channelTools()
	for name, gate := range map[string]func() bool{
		"slack":    ct.SlackHumanGoverned,
		"email":    ct.EmailHumanGoverned,
		"whatsapp": ct.WhatsAppHumanGoverned,
	} {
		if gate == nil {
			t.Fatalf("%s gate must be wired even in ai mode", name)
		}
		if gate() {
			t.Errorf("%s gate must report false while in ai mode", name)
		}
	}

	h.applyGovernanceMode("human") // ##humanoid governance <name> human

	for name, gate := range map[string]func() bool{
		"slack":    ct.SlackHumanGoverned,
		"email":    ct.EmailHumanGoverned,
		"whatsapp": ct.WhatsAppHumanGoverned,
	} {
		if !gate() {
			t.Errorf("%s gate must observe the governance switch without a respawn", name)
		}
	}
}

// TestGovernanceGateClosuresAreRaceFree pins design 019 gap 5: the governance
// fields are written on the actor goroutine but read from tool-execution
// goroutines through the gate closures. Run with -race — before govMu this
// was a detectable data race.
func TestGovernanceGateClosuresAreRaceFree(t *testing.T) {
	h := NewHumanoidActor("racer", "sp", map[string]msg.ChannelConfig{
		"slack":    {},
		"email":    {},
		"whatsapp": {},
	}, config.Config{}, nil, nil, nil, nil)
	ct := h.channelTools()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the actor goroutine flipping modes
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if i%2 == 0 {
				h.applyGovernanceMode("human")
			} else {
				h.applyGovernanceMode("ai")
			}
		}
	}()
	go func() { // a tool-execution goroutine consulting the gates per call
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = ct.SlackHumanGoverned()
			_ = ct.EmailHumanGoverned()
			_ = ct.WhatsAppHumanGoverned()
		}
	}()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// THE approval: the owner's "send" in the pane is what flips a draft to
// approved. Slack has had this since defect 9; email and whatsapp gained it
// together with their code-enforced gates — without it the gate would be a
// wall, not a gate.
// ---------------------------------------------------------------------------

func TestEmailSendCommandApprovesLatestDraft(t *testing.T) {
	h := &HumanoidActor{name: "mail-bot", drafts: channels.NewDraftStore()}
	id := h.drafts.Create("email", "a@example.com", "hi", "draft body", "")

	prompt := h.buildHumanGovernedPrompt("send")
	if !strings.Contains(prompt, id) {
		t.Fatalf("the send prompt must point the model at the approved draft, got: %s", prompt)
	}
	d, _ := h.drafts.Get(id)
	if !d.Approved() {
		t.Fatal("the owner's \"send\" must approve the pending email draft — this IS the approval")
	}

	// A repeated "send" finds nothing pending and says so.
	again := h.buildHumanGovernedPrompt("send")
	if !strings.Contains(again, "nothing awaiting approval") {
		t.Fatalf("a second send must report nothing pending, got: %s", again)
	}
}

func TestWhatsAppSendCommandApprovesLatestDraft(t *testing.T) {
	h := &HumanoidActor{name: "wa-bot", drafts: channels.NewDraftStore()}
	id := h.drafts.Create("whatsapp", "447700900123", "", "draft body", "")

	prompt := h.buildWhatsAppGovernedPrompt("send")
	if !strings.Contains(prompt, id) {
		t.Fatalf("the send prompt must point the model at the approved draft, got: %s", prompt)
	}
	d, _ := h.drafts.Get(id)
	if !d.Approved() {
		t.Fatal("the owner's \"send\" must approve the pending WhatsApp draft — this IS the approval")
	}

	again := h.buildWhatsAppGovernedPrompt("send")
	if !strings.Contains(again, "nothing awaiting approval") {
		t.Fatalf("a second send must report nothing pending, got: %s", again)
	}
}

// TestSendCommandApprovalIsChannelScoped: an email-flow "send" must not
// approve a pending draft on another channel of the same humanoid — the
// store is shared, and the send tools trust ApprovedAt.
func TestSendCommandApprovalIsChannelScoped(t *testing.T) {
	h := &HumanoidActor{name: "multi", drafts: channels.NewDraftStore()}
	slackID := h.drafts.Create("slack", "C123", "", "slack draft", "")

	prompt := h.buildHumanGovernedPrompt("send")
	if !strings.Contains(prompt, "nothing awaiting approval") {
		t.Fatalf("no EMAIL draft is pending, so the email send must report nothing, got: %s", prompt)
	}
	if d, _ := h.drafts.Get(slackID); d.Approved() {
		t.Fatal("an email-flow send must never approve a slack draft")
	}
}
