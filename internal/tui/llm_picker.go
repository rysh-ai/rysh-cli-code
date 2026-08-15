// SPDX-License-Identifier: Apache-2.0

package tui

// The interactive `##llm select` picker — arrow keys instead of "read the
// numbers, then type one of them".
//
// Three steps, the third conditional:
//
//	1. model  — pick from the activatable models in .rysh/llms
//	2. scope  — pick how far the binding reaches: session, workspace, tab,
//	            lane, stack or pane (narrower always wins over broader)
//	3. secret — only when the chosen model's provider has no API key in reach;
//	            type it now, or press esc and set it later
//
// The daemon does not drive any of this. It answers a bare `##llm select` with
// one MsgLLMPickerOpen push (the models, the scopes, and which providers are
// keyless) and returns to its mailbox; see actors.cmdLLMSelect for why it must
// not block. Every step that CHANGES something resolves by submitting an
// ordinary `##` command — `##<scope> model <ref>`, then `##secret new <NAME>
// <value>` — so the picker is a keyboard front-end to commands that already
// exist, not a second way into the model hierarchy. Nothing here validates a
// model, binds a scope, or stores a secret: it types for the user.
//
// Consequences worth keeping: every guard on those commands (the executable
// check, the cross-family provider build, the missing-key warning, the
// secret-echo masking) applies unchanged, the pane's rysh history records what
// happened exactly as if it had been typed, and a front-end that never opens
// the picker still has the numbered menu.

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// llmPickerStep is which of the three steps is on screen.
type llmPickerStep int

const (
	pickerStepModel llmPickerStep = iota
	pickerStepScope
	pickerStepSecret
)

// llmPickerState is the whole picker. One at a time per TUI — it is modal, and
// a second `##llm select` replaces it rather than stacking.
type llmPickerState struct {
	paneID   string
	models   []msgpkg.LLMPickerModel
	blocked  []string
	scopes   []msgpkg.LLMPickerScope
	inEffect string

	step     llmPickerStep
	modelIdx int
	scopeIdx int

	// secretInput collects the API key in step three. Masked on screen, and the
	// command it produces is masked again in the pane's history by the daemon
	// (actors.maskSecretCommandEcho), so the value reaches the secret store
	// without being written anywhere a later reader can see it.
	secretInput textinput.Model
	// chosen* are captured when a step is confirmed, so the later steps read
	// what was actually chosen rather than where the cursor happens to be now.
	// Re-deriving the model from modelIdx would agree today and stop agreeing
	// the moment a step can be revisited without moving the cursor with it.
	chosenModel msgpkg.LLMPickerModel
	chosenRef   string
	chosenScope string
}

// llmPickerOpenMsg is the tea.Msg form of the daemon's push.
type llmPickerOpenMsg struct{ open *msgpkg.MsgLLMPickerOpen }

// setupLLMPickerSubscription wires the pane.*.llm.picker push into a channel
// the Bubble Tea loop drains, mirroring how approvals and email events arrive.
// Buffered and non-blocking: a picker push must never stall the daemon's
// publisher, and dropping one only costs the overlay — the numbered menu that
// was printed alongside it is still on screen.
func setupLLMPickerSubscription(b *bus.Bus) chan *msgpkg.MsgLLMPickerOpen {
	ch := make(chan *msgpkg.MsgLLMPickerOpen, 4)
	codecs := b.Codecs()
	_, _ = b.Conn().Subscribe(msgpkg.T("pane", "*", "llm", "picker"), func(n *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(n.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		open, ok := decoded.(*msgpkg.MsgLLMPickerOpen)
		if !ok {
			return
		}
		select {
		case ch <- open:
		default:
		}
	})
	return ch
}

// listenLLMPickerCmd blocks until the next picker push arrives.
func (m Model) listenLLMPickerCmd() tea.Cmd {
	ch := m.llmPickerCh
	return func() tea.Msg { return llmPickerOpenMsg{open: <-ch} }
}

// openLLMPicker installs a pushed payload as the live picker, or declines it.
//
// Three reasons to decline, all of which leave the user with the numbered menu
// that was printed alongside the push — so declining costs the overlay, never
// the command:
//
//   - nothing to arrow through (no activatable models, or no scopes);
//   - a PTY relay owns the terminal, so Bubble Tea is suspended and would not
//     see a single key of the picker it just opened;
//   - the push names a pane this TUI is not focused on. The subject is
//     session-scoped, so a second attached front-end — or a cron-fired
//     `##llm select` — reaches every TUI in the session; only the one whose
//     user is actually in that pane should have its keyboard taken over.
func (m *Model) openLLMPicker(open *msgpkg.MsgLLMPickerOpen) {
	if open == nil || len(open.Models) == 0 || len(open.Scopes) == 0 {
		return
	}
	if m.relayPaneID != "" {
		return
	}
	if open.PaneID != "" && m.snapshot.ActivePaneID != "" && open.PaneID != m.snapshot.ActivePaneID {
		return
	}
	in := textinput.New()
	in.Placeholder = "paste or type the key"
	in.EchoMode = textinput.EchoPassword
	in.CharLimit = 500
	st := &llmPickerState{
		paneID:      open.PaneID,
		models:      open.Models,
		blocked:     open.Blocked,
		scopes:      open.Scopes,
		inEffect:    open.InEffect,
		secretInput: in,
	}
	// Start on the model already in effect, so "just confirm twice" re-binds
	// what is running rather than whatever happens to sort first.
	for i, mdl := range open.Models {
		if mdl.Current {
			st.modelIdx = i
			break
		}
	}
	m.llmPicker = st
	m.mode = modeLLMPicker
}

// closeLLMPicker drops the picker and hands the keyboard back to the pane.
func (m *Model) closeLLMPicker() {
	m.llmPicker = nil
	m.mode = modeNormal
	m.syncPaneInputFocus()
}

// bindCommand is the `##` command that binds the picked model at the picked
// scope — `##tab model openai/luna`. Pure, because the exact text is the whole
// contract between the picker and the daemon: get it wrong and the picker binds
// the wrong scope, or nothing at all, with no type to catch it.
func (p *llmPickerState) bindCommand() string {
	if p == nil || p.scopeIdx >= len(p.scopes) || p.chosenRef == "" {
		return ""
	}
	return p.scopes[p.scopeIdx].Command + " " + p.chosenRef
}

// secretCommand is the `##secret new <NAME> <value>` command for the key typed
// in step three, or "" when there is nothing to store. The daemon masks this
// line before it reaches the pane's history (actors.maskSecretCommandEcho).
func (p *llmPickerState) secretCommand() string {
	if p == nil {
		return ""
	}
	value := strings.TrimSpace(p.secretInput.Value())
	name := p.chosenModel.KeyName
	if value == "" || name == "" {
		return ""
	}
	return fmt.Sprintf("##secret new %s %s", name, value)
}

// submitPickerCommand submits one `##` command as if it had been typed into the
// picker's pane, which is what makes the picker a front-end rather than a
// second implementation of the binding.
func (m Model) submitPickerCommand(text string) {
	if strings.TrimSpace(text) == "" || m.pub == nil {
		return
	}
	paneID := ""
	if m.llmPicker != nil {
		paneID = m.llmPicker.paneID
	}
	if paneID == "" {
		paneID = m.snapshot.ActivePaneID
	}
	m.sendMsg(&msgpkg.MsgSubmitInput{Text: text, Mode: "rysh", PaneID: paneID})
}

// ---- Key handling (m.mode == modeLLMPicker) -------------------------------

// updateLLMPickerMode owns every key while the picker is open.
func (m Model) updateLLMPickerMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.llmPicker == nil {
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	}
	if m.llmPicker.step == pickerStepSecret {
		return m.updateLLMPickerSecret(msg)
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		// Esc in step two goes back to the model list rather than abandoning
		// the whole picker — the scope is the easier choice to get wrong.
		if m.llmPicker.step == pickerStepScope {
			m.llmPicker.step = pickerStepModel
			return m, nil
		}
		m.closeLLMPicker()
		return m, nil
	case "up", "k", "shift+tab":
		m.llmPickerMove(-1)
		return m, nil
	case "down", "j", "tab":
		m.llmPickerMove(1)
		return m, nil
	case "home", "g":
		m.llmPickerSet(0)
		return m, nil
	case "end", "G":
		m.llmPickerSet(m.llmPickerLen() - 1)
		return m, nil
	case "enter", " ":
		return m.confirmLLMPickerStep()
	}
	return m, nil
}

// updateLLMPickerSecret owns the keys of step three: the masked key entry.
func (m Model) updateLLMPickerSecret(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// "Set it later" — the binding already happened; only the key is
		// deferred, and the daemon's own warning already said what is missing.
		m.closeLLMPicker()
		return m, tea.Batch(m.refreshCmd(), m.listenLLMPickerCmd())
	case "enter":
		// An empty key is not a key: secretCommand returns "" and this becomes
		// "later" rather than storing a blank that would fail authentication
		// just as surely, but silently and one step further from the cause.
		m.submitPickerCommand(m.llmPicker.secretCommand())
		m.llmPicker.secretInput.SetValue("")
		m.closeLLMPicker()
		return m, tea.Batch(m.refreshCmd(), m.listenLLMPickerCmd())
	}
	var cmd tea.Cmd
	m.llmPicker.secretInput, cmd = m.llmPicker.secretInput.Update(msg)
	return m, cmd
}

// confirmLLMPickerStep advances the picker one step, binding the model when the
// scope is chosen.
func (m Model) confirmLLMPickerStep() (tea.Model, tea.Cmd) {
	p := m.llmPicker
	switch p.step {
	case pickerStepModel:
		p.chosenModel = p.models[p.modelIdx]
		p.chosenRef = p.chosenModel.Ref
		p.step = pickerStepScope
		return m, nil
	case pickerStepScope:
		p.chosenScope = p.scopes[p.scopeIdx].Name
		m.submitPickerCommand(p.bindCommand())
		// Step three only exists when the daemon told us this model's provider
		// has no key in reach. Otherwise the picker is done.
		if !p.chosenModel.KeyMissing {
			m.closeLLMPicker()
			return m, tea.Batch(m.refreshCmd(), m.listenLLMPickerCmd())
		}
		p.step = pickerStepSecret
		p.secretInput.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

// llmPickerLen / llmPickerSet / llmPickerMove drive whichever list is on
// screen, so the navigation keys are written once.
func (m Model) llmPickerLen() int {
	if m.llmPicker.step == pickerStepScope {
		return len(m.llmPicker.scopes)
	}
	return len(m.llmPicker.models)
}

func (m Model) llmPickerSet(i int) {
	n := m.llmPickerLen()
	if n == 0 {
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	if m.llmPicker.step == pickerStepScope {
		m.llmPicker.scopeIdx = i
		return
	}
	m.llmPicker.modelIdx = i
}

// llmPickerMove steps by delta and WRAPS: a six-item scope list is faster to
// reach the end of by pressing up once.
func (m Model) llmPickerMove(delta int) {
	n := m.llmPickerLen()
	if n == 0 {
		return
	}
	cur := m.llmPicker.modelIdx
	if m.llmPicker.step == pickerStepScope {
		cur = m.llmPicker.scopeIdx
	}
	m.llmPickerSet(((cur+delta)%n + n) % n)
}

// pickerKeyName is the variable step three collects, e.g. OPENAI_API_KEY.
func (m Model) pickerKeyName() string {
	if name := m.llmPicker.chosenModel.KeyName; name != "" {
		return name
	}
	return "API_KEY"
}

// ---- Rendering ------------------------------------------------------------

// renderLLMPicker draws the picker as a bordered panel sized to the body. It
// REPLACES the pane grid rather than floating over it: compositing a box over
// live PTY output means clipping styled cells mid-escape-sequence, and the
// picker is modal anyway — nothing behind it can be interacted with.
func (m Model) renderLLMPicker(width, height int) string {
	p := m.llmPicker
	if p == nil {
		return ""
	}
	var b strings.Builder
	// The step count is knowable in advance: the daemon already told us whether
	// the chosen model's provider needs a key, so the header never promises
	// "of 2" and then asks for a third thing.
	total := 2
	if (p.step == pickerStepModel && p.models[p.modelIdx].KeyMissing) ||
		(p.step != pickerStepModel && p.chosenModel.KeyMissing) {
		total = 3
	}
	title := map[llmPickerStep]string{
		pickerStepModel:  fmt.Sprintf("select a model  (step 1 of %d)", total),
		pickerStepScope:  fmt.Sprintf("where should it apply?  (step 2 of %d)", total),
		pickerStepSecret: fmt.Sprintf("this provider has no API key  (step 3 of %d)", total),
	}[p.step]
	fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Render("## llm — "+title))
	if p.inEffect != "" && p.step != pickerStepSecret {
		fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Faint(true).Render("in effect now: "+p.inEffect))
	}
	b.WriteString("\n")

	switch p.step {
	case pickerStepModel:
		b.WriteString(m.renderPickerModels())
	case pickerStepScope:
		b.WriteString(m.renderPickerScopes())
	case pickerStepSecret:
		b.WriteString(m.renderPickerSecret())
	}

	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(max(20, width-4)).
		Render(strings.TrimRight(b.String(), "\n"))
	// Vertically centred in the body so the panel does not jump as the step's
	// height changes.
	return lipgloss.Place(max(20, width), max(3, height), lipgloss.Center, lipgloss.Center, panel)
}

func (m Model) renderPickerModels() string {
	p := m.llmPicker
	refWidth := 0
	for _, mdl := range p.models {
		if len(mdl.Ref) > refWidth {
			refWidth = len(mdl.Ref)
		}
	}
	var b strings.Builder
	for i, mdl := range p.models {
		cursor, current := "  ", " "
		if i == p.modelIdx {
			cursor = "▸ "
		}
		if mdl.Current {
			current = ">"
		}
		row := fmt.Sprintf("%s%s %-*s  %s", cursor, current, refWidth, mdl.Ref, mdl.Model)
		if mdl.KeyMissing {
			row += "  [no " + mdl.KeyName + "]"
		}
		if i == p.modelIdx {
			row = lipgloss.NewStyle().Bold(true).Render(row)
		}
		b.WriteString(row + "\n")
	}
	for _, blk := range p.blocked {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render("    "+blk) + "\n")
	}
	return b.String()
}

func (m Model) renderPickerScopes() string {
	p := m.llmPicker
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", lipgloss.NewStyle().Faint(true).Render("binding "+p.chosenRef))
	nameWidth := 0
	for _, s := range p.scopes {
		if len(s.Name) > nameWidth {
			nameWidth = len(s.Name)
		}
	}
	for i, s := range p.scopes {
		cursor := "  "
		if i == p.scopeIdx {
			cursor = "▸ "
		}
		row := fmt.Sprintf("%s%-*s  %s", cursor, nameWidth, s.Name, s.Hint)
		if i == p.scopeIdx {
			row = lipgloss.NewStyle().Bold(true).Render(row)
		}
		b.WriteString(row + "\n")
	}
	return b.String()
}

func (m Model) renderPickerSecret() string {
	p := m.llmPicker
	var b strings.Builder
	fmt.Fprintf(&b, "%s is bound at %s scope, but %s has no key in reach —\n",
		p.chosenRef, p.chosenScope, p.chosenModel.Provider)
	fmt.Fprintf(&b, "%s\n\n", lipgloss.NewStyle().Faint(true).Render(
		"neither ##secret nor the daemon environment. Prompts will fail until it is set."))
	fmt.Fprintf(&b, "%s\n%s\n", m.pickerKeyName(), p.secretInput.View())
	return b.String()
}

// llmPickerFooter is the footer hint for each step.
func (m Model) llmPickerFooter() string {
	if m.llmPicker == nil {
		return ""
	}
	switch m.llmPicker.step {
	case pickerStepScope:
		return "llm picker: scope | ↑/↓ or k/j move  enter bind  esc back to models"
	case pickerStepSecret:
		return "llm picker: api key | type the key (hidden)  enter save to ##secret  esc set it later"
	default:
		return "llm picker: model | ↑/↓ or k/j move  enter choose scope  esc cancel"
	}
}
