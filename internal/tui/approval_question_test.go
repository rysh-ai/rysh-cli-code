// SPDX-License-Identifier: Apache-2.0

package tui

// F-92 in the TUI. ask_user's question arrives here as an approval request of
// type "question", and the TUI answers it with the gate keys. The decisions
// are right once ask_user maps them (y → "yes", n/Esc → declined, N → the typed
// text), but what the pane shows and what an empty N sends were still gate
// words: the echo printed [no_with_explanation: …], an empty N sent the answer
// "rejected by user", and the footer offered approve/reject.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func questionModel(t *testing.T, reqType msgpkg.ApprovalType) (Model, *nats.Subscription, *nats.Subscription) {
	t.Helper()
	nc := recorderNATS(t)
	resp, err := nc.SubscribeSync(msgpkg.T("pane", "p1", "approval", "response"))
	if err != nil {
		t.Fatal(err)
	}
	echo, err := nc.SubscribeSync(msgpkg.T("pane", "p1", "output", string(msgpkg.ConvAI)))
	if err != nil {
		t.Fatal(err)
	}
	m := Model{
		pub:             msgpkg.NewNATSPublisher(nc, msgpkg.DefaultCodecRegistry()),
		bus:             &bus.Bus{}, // refreshCmd builds a cmd from it; never run here
		approvalCh:      make(chan approvalRequestMsg, 1),
		mode:            modeApproval,
		pendingApproval: &msgpkg.MsgApprovalRequest{RequestID: "req-q", Type: reqType, Description: "Which database?"},
		approvalPaneID:  "p1",
		rejectInput:     textinput.New(),
		inputs:          map[string]textinput.Model{},
		paneInputModes:  map[string]string{},
	}
	return m, resp, echo
}

func nextResponse(t *testing.T, sub *nats.Subscription) msgpkg.MsgApprovalResponse {
	t.Helper()
	m, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no approval response: %v", err)
	}
	var env msgpkg.NATSEnvelope
	var r msgpkg.MsgApprovalResponse
	if json.Unmarshal(m.Data, &env) != nil || json.Unmarshal(env.Payload, &r) != nil {
		t.Fatal("decode response")
	}
	return r
}

func nextEcho(t *testing.T, sub *nats.Subscription) string {
	t.Helper()
	m, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no echo: %v", err)
	}
	var env msgpkg.NATSEnvelope
	var c msgpkg.MsgConversationAppend
	if json.Unmarshal(m.Data, &env) != nil || json.Unmarshal(env.Payload, &c) != nil || c.Message == nil {
		t.Fatal("decode echo")
	}
	return c.Message.Content
}

func qkey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestTUIQuestionKeysEchoAnswers(t *testing.T) {
	for _, c := range []struct{ key, decision, echo string }{
		{"y", "yes", "[answered: yes]\n"},
		{"Y", "yes_always", "[answered: yes]\n"},
		{"n", "no", "[declined]\n"},
		{"esc", "no", "[declined]\n"},
	} {
		t.Run(c.key, func(t *testing.T) {
			m, resp, echo := questionModel(t, msgpkg.ApprovalTypeQuestion)
			_, _ = m.updateApprovalMode(qkey(c.key))
			if r := nextResponse(t, resp); string(r.Decision) != c.decision {
				t.Errorf("decision = %q, want %q", r.Decision, c.decision)
			}
			if got := nextEcho(t, echo); got != c.echo {
				t.Errorf("echo = %q, want %q", got, c.echo)
			}
		})
	}
}

func TestTUIQuestionTypedAnswer(t *testing.T) {
	m, resp, echo := questionModel(t, msgpkg.ApprovalTypeQuestion)
	mm, _ := m.updateApprovalMode(qkey("N"))
	m = mm.(Model)
	m.rejectInput.SetValue("use postgres")
	_, _ = m.updateRejectReasonMode(qkey("enter"))
	if r := nextResponse(t, resp); r.Reason != "use postgres" {
		t.Errorf("reason = %q, want the typed answer", r.Reason)
	}
	if got := nextEcho(t, echo); got != "[answered: use postgres]\n" {
		t.Errorf("echo = %q", got)
	}
}

// An empty N on a question is a decline. It used to send the literal answer
// "rejected by user", which ask_user handed to the model as the user's words.
func TestTUIQuestionEmptyAnswerDeclines(t *testing.T) {
	m, resp, echo := questionModel(t, msgpkg.ApprovalTypeQuestion)
	mm, _ := m.updateApprovalMode(qkey("N"))
	m = mm.(Model)
	_, _ = m.updateRejectReasonMode(qkey("enter"))
	r := nextResponse(t, resp)
	if r.Decision != msgpkg.DecisionNo || r.Reason != "" {
		t.Errorf("empty answer sent %q / %q, want a plain no", r.Decision, r.Reason)
	}
	if got := nextEcho(t, echo); got != "[declined]\n" {
		t.Errorf("echo = %q", got)
	}
}

func TestTUIQuestionFooterIsNotAnApproval(t *testing.T) {
	m, _, _ := questionModel(t, msgpkg.ApprovalTypeQuestion)
	help := m.keyHelp()
	if strings.Contains(help, "APPROVAL") || strings.Contains(help, "approve") {
		t.Errorf("question footer reads as a gate: %q", help)
	}
	if !strings.Contains(help, "Which database?") {
		t.Errorf("question footer lost the question: %q", help)
	}
}

// A gate is unchanged: echo is the decision, footer is the approval prompt.
func TestTUIGateUnchanged(t *testing.T) {
	m, _, echo := questionModel(t, msgpkg.ApprovalTypeDestructive)
	if !strings.Contains(m.keyHelp(), "APPROVAL") {
		t.Errorf("gate footer changed: %q", m.keyHelp())
	}
	_, _ = m.updateApprovalMode(qkey("y"))
	if got := nextEcho(t, echo); got != "[yes]\n" {
		t.Errorf("gate echo = %q, want [yes]", got)
	}
}

// F-92 residue: a question used to open in the gate's approval mode, so typing
// an answer meant N first — and a "y" typed as the first letter of "yes" sent
// Approve. A question now opens straight into an answer field. These go
// through Update with a real approvalRequestMsg, the path a live request takes.

func questionArrives(t *testing.T, choices []msgpkg.Choice) (Model, *nats.Subscription, *nats.Subscription) {
	t.Helper()
	m, resp, echo := questionModel(t, msgpkg.ApprovalTypeQuestion)
	m.mode = modeNormal
	m.pendingApproval = nil
	m.approvalPaneID = ""
	m.snapshot = domain.WorkspaceSnapshot{
		ActivePaneID: "p1",
		Tabs: []domain.TabSnapshot{{ID: "t1", Lanes: []domain.LaneSnapshot{{ID: "l1",
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "g1", ActivePaneID: "p1", Panes: []domain.PaneSnapshot{{ID: "p1"}}}}}}}},
	}
	mm, _ := m.Update(approvalRequestMsg{PaneID: "p1", Request: &msgpkg.MsgApprovalRequest{
		RequestID: "req-q", Type: msgpkg.ApprovalTypeQuestion, Description: "Which database?", Choices: choices,
	}})
	return mm.(Model), resp, echo
}

func typeInto(m Model, s string) Model {
	for _, r := range s {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(Model)
	}
	return m
}

func noResponse(t *testing.T, sub *nats.Subscription) {
	t.Helper()
	if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("unexpected approval response: %s", m.Data)
	}
}

func TestTUIQuestionOpensIntoAnAnswerField(t *testing.T) {
	m, resp, echo := questionArrives(t, nil)
	if m.mode != modeQuestion {
		t.Fatalf("question opened in mode %q, want %q", m.mode, modeQuestion)
	}
	// "yes" typed letter by letter is text, not an Approve on the "y".
	m = typeInto(m, "yes, use postgres")
	noResponse(t, resp)
	mm, _ := m.Update(qkey("enter"))
	m = mm.(Model)
	r := nextResponse(t, resp)
	if r.Decision != msgpkg.DecisionYes || r.Reason != "yes, use postgres" {
		t.Errorf("sent %q / %q, want yes + the typed answer", r.Decision, r.Reason)
	}
	if got := nextEcho(t, echo); got != "[answered: yes, use postgres]\n" {
		t.Errorf("echo = %q", got)
	}
	if m.mode != modeNormal || m.pendingApproval != nil {
		t.Errorf("after answering: mode %q, pending %v", m.mode, m.pendingApproval)
	}
}

func TestTUIQuestionEscDeclines(t *testing.T) {
	m, resp, echo := questionArrives(t, nil)
	m = typeInto(m, "half an ans")
	_, _ = m.Update(qkey("esc"))
	if r := nextResponse(t, resp); r.Decision != msgpkg.DecisionNo || r.Reason != "" {
		t.Errorf("esc sent %q / %q, want a plain no", r.Decision, r.Reason)
	}
	if got := nextEcho(t, echo); got != "[declined]\n" {
		t.Errorf("echo = %q", got)
	}
}

// An empty Enter is ignored, not a decline: Esc is the explicit decline, and a
// stray Enter (the habit after reading the question) must not answer for the
// user. The app's Send is disabled on an empty answer for the same reason.
func TestTUIQuestionEmptyEnterIsIgnored(t *testing.T) {
	m, resp, _ := questionArrives(t, nil)
	mm, _ := m.Update(qkey("enter"))
	noResponse(t, resp)
	if mm.(Model).mode != modeQuestion {
		t.Errorf("empty enter left the answer field (mode %q)", mm.(Model).mode)
	}
}

// With options, the answer field also picks one: a number 1..N or an option's
// exact label sends choice_selected with ChoiceIdx (F-47), anything else is a
// free-text answer.
func TestTUIQuestionPicksAnOption(t *testing.T) {
	opts := []msgpkg.Choice{{Label: "postgres"}, {Label: "sqlite"}}
	for _, c := range []struct{ typed, label string }{{"2", "sqlite"}, {"sqlite", "sqlite"}, {"1", "postgres"}} {
		t.Run(c.typed, func(t *testing.T) {
			m, resp, echo := questionArrives(t, opts)
			m = typeInto(m, c.typed)
			_, _ = m.Update(qkey("enter"))
			r := nextResponse(t, resp)
			want := 0
			if c.label == "sqlite" {
				want = 1
			}
			if r.Decision != msgpkg.DecisionChoiceSelected || r.ChoiceIdx != want || r.Reason != c.label {
				t.Errorf("typed %q sent %q idx %d %q, want choice_selected idx %d %q", c.typed, r.Decision, r.ChoiceIdx, r.Reason, want, c.label)
			}
			if got := nextEcho(t, echo); got != fmt.Sprintf("[chose option %d: %s]\n", want+1, c.label) {
				t.Errorf("echo = %q", got)
			}
		})
	}
	t.Run("free text with options", func(t *testing.T) {
		m, resp, _ := questionArrives(t, opts)
		m = typeInto(m, "3 replicas")
		_, _ = m.Update(qkey("enter"))
		if r := nextResponse(t, resp); r.Decision != msgpkg.DecisionYes || r.Reason != "3 replicas" {
			t.Errorf("sent %q / %q", r.Decision, r.Reason)
		}
	})
}

// A gate still opens in approval mode.
func TestTUIGateStillOpensInApprovalMode(t *testing.T) {
	m, _, _ := questionArrives(t, nil)
	_ = m
	g, _, _ := questionModel(t, msgpkg.ApprovalTypeDestructive)
	g.mode, g.pendingApproval = modeNormal, nil
	g.snapshot = m.snapshot
	mm, _ := g.Update(approvalRequestMsg{PaneID: "p1", Request: &msgpkg.MsgApprovalRequest{RequestID: "g", Type: msgpkg.ApprovalTypeDestructive}})
	if mm.(Model).mode != modeApproval {
		t.Errorf("gate opened in mode %q", mm.(Model).mode)
	}
}
