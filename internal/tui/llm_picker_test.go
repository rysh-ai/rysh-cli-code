package tui

// The `##llm select` picker is a keyboard front-end to commands that already
// exist, so what needs pinning is the state machine and the exact command text
// it produces — a picker that binds the wrong scope, or nothing, fails silently
// otherwise.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func pickerPayload() *msgpkg.MsgLLMPickerOpen {
	return &msgpkg.MsgLLMPickerOpen{
		PaneID: "p1",
		Models: []msgpkg.LLMPickerModel{
			{Ref: "anthropic/fable5", Model: "claude-fable-5", Provider: "anthropic"},
			{Ref: "openai/luna", Model: "gpt-5.6-luna", Provider: "openai",
				KeyMissing: true, KeyName: "OPENAI_API_KEY"},
			{Ref: "openai/sol", Model: "gpt-5.6-sol", Provider: "openai", Current: true},
		},
		Scopes: []msgpkg.LLMPickerScope{
			{Name: "session", Command: "##session model"},
			{Name: "workspace", Command: "##workspace model"},
			{Name: "tab", Command: "##tab model"},
			{Name: "pane", Command: "##pane model"},
		},
		InEffect: "openai/sol",
	}
}

// pickerModel is a Model with the picker open on the payload above, over a real
// embedded bus so the submit paths resolve (refreshCmd reaches for the bus's
// codecs even when the command it builds is never run).
func pickerModel(t *testing.T) Model {
	t.Helper()
	b := startTestBus(t)
	m := Model{
		pub:            b.Publisher(),
		bus:            b,
		workspaceInbox: msgpkg.T("ws", "inbox"),
		width:          100,
		height:         30,
	}
	m.snapshot.ActivePaneID = "p1"
	m.openLLMPicker(pickerPayload())
	if m.llmPicker == nil {
		t.Fatal("picker did not open")
	}
	return m
}

// capturedCommands subscribes to the workspace inbox and returns a func that
// drains whatever the picker submitted — the picker's whole output contract is
// the `##` commands it types on the user's behalf.
func capturedCommands(t *testing.T, b *bus.Bus) func() []string {
	t.Helper()
	ch := make(chan string, 8)
	codecs := b.Codecs()
	sub, err := b.Conn().Subscribe(msgpkg.T("ws", "inbox"), func(n *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(n.Data, &env) != nil {
			return
		}
		d, derr := codecs.Decode(env.TypeTag, env.Payload)
		if derr != nil {
			return
		}
		if in, ok := d.(*msgpkg.MsgSubmitInput); ok {
			ch <- in.Text
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return func() []string {
		var got []string
		deadline := time.After(2 * time.Second)
		for {
			select {
			case s := <-ch:
				got = append(got, s)
			case <-time.After(150 * time.Millisecond):
				return got
			case <-deadline:
				return got
			}
		}
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press feeds one key to the picker and returns the resulting Model.
func press(m Model, k string) Model {
	next, _ := m.updateLLMPickerMode(key(k))
	return next.(Model)
}

// TestPickerOpensOnCurrentModel: the cursor starts on the model already in
// effect, so confirming twice re-binds what is running instead of whatever
// happens to sort first.
func TestPickerOpensOnCurrentModel(t *testing.T) {
	m := pickerModel(t)
	if m.mode != modeLLMPicker {
		t.Fatalf("mode = %q, want %q", m.mode, modeLLMPicker)
	}
	if got := m.llmPicker.models[m.llmPicker.modelIdx].Ref; got != "openai/sol" {
		t.Errorf("cursor on %q, want the in-effect openai/sol", got)
	}
}

// TestPickerNavigationWraps: arrows and vi keys move the same list, and the
// ends wrap so a six-item scope list is one keypress deep from either side.
func TestPickerNavigationWraps(t *testing.T) {
	m := pickerModel(t)
	m.llmPicker.modelIdx = 0

	m = press(m, "down")
	if m.llmPicker.modelIdx != 1 {
		t.Errorf("down: idx = %d, want 1", m.llmPicker.modelIdx)
	}
	m = press(m, "k") // vi up
	if m.llmPicker.modelIdx != 0 {
		t.Errorf("k: idx = %d, want 0", m.llmPicker.modelIdx)
	}
	m = press(m, "up") // wrap backwards to the last model
	if want := len(m.llmPicker.models) - 1; m.llmPicker.modelIdx != want {
		t.Errorf("wrap up: idx = %d, want %d", m.llmPicker.modelIdx, want)
	}
	m = press(m, "j") // wrap forwards to the first
	if m.llmPicker.modelIdx != 0 {
		t.Errorf("wrap down: idx = %d, want 0", m.llmPicker.modelIdx)
	}
}

// TestPickerBindsChosenScope walks both steps and pins the command text. The
// scope list must drive the command, not the model step — binding at the wrong
// level is the failure this picker exists to make easy to avoid.
func TestPickerBindsChosenScope(t *testing.T) {
	m := pickerModel(t)
	m.llmPicker.modelIdx = 0 // anthropic/fable5 — a provider with a key

	m = press(m, "enter")
	if m.llmPicker.step != pickerStepScope {
		t.Fatalf("step = %v, want scope", m.llmPicker.step)
	}
	if m.llmPicker.chosenRef != "anthropic/fable5" {
		t.Fatalf("chosenRef = %q", m.llmPicker.chosenRef)
	}

	m = press(m, "down") // session → workspace
	m = press(m, "down") // workspace → tab
	if got := m.llmPicker.bindCommand(); got != "##tab model anthropic/fable5" {
		t.Errorf("bindCommand = %q, want ##tab model anthropic/fable5", got)
	}

	drain := capturedCommands(t, m.bus)
	m = press(m, "enter")
	// The provider has a key, so there is no third step: the picker is done.
	if m.llmPicker != nil {
		t.Errorf("picker stayed open at step %v after binding a keyed provider", m.llmPicker.step)
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %q, want normal", m.mode)
	}
	// The command must actually reach the workspace — the picker changes
	// nothing itself, so a dropped submission is a silent no-op.
	sent := drain()
	if len(sent) != 1 || sent[0] != "##tab model anthropic/fable5" {
		t.Errorf("submitted %q, want [##tab model anthropic/fable5]", sent)
	}
}

// TestPickerAsksForMissingKey: choosing a model whose provider has no key in
// reach opens the third step instead of finishing.
func TestPickerAsksForMissingKey(t *testing.T) {
	m := pickerModel(t)
	m.llmPicker.modelIdx = 1 // openai/luna — KeyMissing

	m = press(m, "enter") // step 1 → 2
	m = press(m, "enter") // bind at session (the default scope row)
	if m.llmPicker == nil {
		t.Fatal("picker closed without asking for the missing key")
	}
	if m.llmPicker.step != pickerStepSecret {
		t.Fatalf("step = %v, want secret", m.llmPicker.step)
	}
	if m.llmPicker.chosenScope != "session" {
		t.Errorf("chosenScope = %q, want session", m.llmPicker.chosenScope)
	}

	m.llmPicker.secretInput.SetValue("sk-typed-by-user")
	if got := m.llmPicker.secretCommand(); got != "##secret new OPENAI_API_KEY sk-typed-by-user" {
		t.Errorf("secretCommand = %q", got)
	}

	m = press(m, "enter")
	if m.llmPicker != nil || m.mode != modeNormal {
		t.Error("picker did not close after saving the key")
	}
}

// TestPickerSetItLater: esc on the key step closes the picker WITHOUT storing
// anything. The binding already happened one step earlier and must stand.
func TestPickerSetItLater(t *testing.T) {
	m := pickerModel(t)
	m.llmPicker.modelIdx = 1
	m = press(m, "enter")
	m = press(m, "enter")
	if m.llmPicker.step != pickerStepSecret {
		t.Fatalf("precondition: step = %v", m.llmPicker.step)
	}
	if got := m.llmPicker.secretCommand(); got != "" {
		t.Errorf("an untouched key field produced %q, want no command", got)
	}
	m = press(m, "esc")
	if m.llmPicker != nil || m.mode != modeNormal {
		t.Error("esc did not close the picker")
	}
}

// TestPickerEmptyKeyIsNotAKey: confirming a blank field stores nothing rather
// than a blank secret, which would fail authentication just as surely but
// further from the cause.
func TestPickerEmptyKeyIsNotAKey(t *testing.T) {
	m := pickerModel(t)
	m.llmPicker.modelIdx = 1
	m = press(m, "enter")
	m = press(m, "enter")
	m.llmPicker.secretInput.SetValue("   ")
	if got := m.llmPicker.secretCommand(); got != "" {
		t.Errorf("blank key produced %q, want no command", got)
	}
}

// TestPickerEscStepsBackThenOut: esc in the scope step returns to the models
// (the easier choice to get wrong), and esc in the model step cancels.
func TestPickerEscStepsBackThenOut(t *testing.T) {
	m := pickerModel(t)
	m = press(m, "enter")
	m = press(m, "esc")
	if m.llmPicker == nil {
		t.Fatal("esc from the scope step closed the picker instead of stepping back")
	}
	if m.llmPicker.step != pickerStepModel {
		t.Fatalf("step = %v, want model", m.llmPicker.step)
	}
	m = press(m, "esc")
	if m.llmPicker != nil || m.mode != modeNormal {
		t.Error("esc from the model step did not cancel the picker")
	}
}

// TestPickerDeclinesForeignAndRelayedPushes: the picker subject is
// session-scoped, so every attached TUI sees every push. Only the one whose
// user is in that pane may take over the keyboard, and none may while a PTY
// relay owns the terminal.
func TestPickerDeclinesForeignAndRelayedPushes(t *testing.T) {
	other := Model{}
	other.snapshot.ActivePaneID = "p2"
	other.openLLMPicker(pickerPayload())
	if other.llmPicker != nil {
		t.Error("picker opened for a pane this TUI is not focused on")
	}

	relayed := Model{relayPaneID: "p1"}
	relayed.snapshot.ActivePaneID = "p1"
	relayed.openLLMPicker(pickerPayload())
	if relayed.llmPicker != nil {
		t.Error("picker opened while a relay owned the terminal")
	}

	empty := Model{}
	empty.snapshot.ActivePaneID = "p1"
	empty.openLLMPicker(&msgpkg.MsgLLMPickerOpen{PaneID: "p1"})
	if empty.llmPicker != nil {
		t.Error("picker opened with nothing to arrow through")
	}
}

// TestPickerRendersWithoutPanicking exercises each step's view at a realistic
// size — a picker that crashes the TUI on render is worse than no picker.
func TestPickerRendersWithoutPanicking(t *testing.T) {
	m := pickerModel(t)
	for _, step := range []llmPickerStep{pickerStepModel, pickerStepScope, pickerStepSecret} {
		m.llmPicker.step = step
		m.llmPicker.chosenRef = "openai/luna"
		m.llmPicker.chosenScope = "tab"
		out := m.renderLLMPicker(80, 24)
		if strings.TrimSpace(out) == "" {
			t.Errorf("step %v rendered nothing", step)
		}
		if strings.TrimSpace(m.llmPickerFooter()) == "" {
			t.Errorf("step %v has no footer hint", step)
		}
	}
}

// TestPickerUsesTheChosenModelNotTheCursor: step three reads what was chosen in
// step one. Re-deriving it from the cursor agrees today only because the scope
// step cannot move it — a coupling worth not depending on.
func TestPickerUsesTheChosenModelNotTheCursor(t *testing.T) {
	m := pickerModel(t)
	m.llmPicker.modelIdx = 1 // openai/luna, the keyless one
	m = press(m, "enter")    // choose it
	m.llmPicker.modelIdx = 0 // cursor drifts elsewhere
	m = press(m, "enter")    // bind at session

	if m.llmPicker == nil || m.llmPicker.step != pickerStepSecret {
		t.Fatal("the chosen model's missing key did not open step three")
	}
	m.llmPicker.secretInput.SetValue("sk-x")
	if got := m.llmPicker.secretCommand(); got != "##secret new OPENAI_API_KEY sk-x" {
		t.Errorf("secretCommand = %q, want the CHOSEN model's key name", got)
	}
}
