// SPDX-License-Identifier: Apache-2.0

package actors

// attention.go — "needs you" detection (root-order-044 #3).
//
// Three sources, from most to least exact:
//
//   - approval: a tool call is held at the approval gate
//     (pane.<id>.approval.request until the matching response);
//   - question: ask_user is waiting (the same subject, Type question);
//   - prompt:   an agent CLI in a raw pane (claude, codex, gemini) shows a
//     permission or question prompt in the bottom rows of its screen.
//
// The first two are events. The third is screen matching, which this codebase
// is right to distrust (see ansaScreenIsWorking), so it is narrow on purpose:
// it needs a foreground program, a quiet screen, a question line AND an answer
// line near the bottom, and no "esc to interrupt" footer. A miss leaves the
// pane unmarked, exactly as it is today; nothing is ever decided by it.
//
// READ-ONLY. Nothing here writes to the PTY, answers, dismisses or delays a
// prompt, and no screen text leaves this file: the state carries a reason
// word, never what the screen said (screens can hold secrets).

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// attentionQuiet is how long the screen must be still before it is read:
	// an agent mid-render is not waiting on anyone.
	attentionQuiet = 500 * time.Millisecond
	// attentionTick is how often the watcher looks at the timestamps. A screen
	// is read only when there was output since the last read.
	attentionTick = 300 * time.Millisecond
	// attentionWindow is how many non-blank bottom rows may hold the question;
	// attentionTail is how many may hold the answer line.
	attentionWindow = 15
	attentionTail   = 6
)

// Question lines, by CLI. Matched as substrings within the window.
var attentionQuestions = []string{
	"Do you want to",                // claude: proceed / make this edit / create
	"Would you like to",             // codex: run the following command / make the following edits
	"Allow command?",                // codex (older builds)
	"Allow execution",               // gemini: shell
	"Apply this change?",            // gemini: edit
	"Waiting for user confirmation", // gemini
}

// Answer lines: option 1 "Yes" behind a selection marker, or one of the
// CLIs' own refusal/allow wordings. Matched within the tail.
var (
	attentionOptionYes = regexp.MustCompile(`(^|[\s│|❯›>●*])1\.\s*Yes\b`)
	attentionAnswers   = []string{
		"No, and tell",        // claude, codex
		"No, suggest changes", // gemini
		"Yes, allow once",     // gemini
		"Yes, proceed",        // codex
	}
)

// attentionMenuFooter is claude's selection-menu footer (AskUserQuestion and
// the like): a menu at the bottom of the screen is waiting on a choice.
const attentionMenuFooter = "Enter to select"

// detectAgentPrompt reports whether the bottom of a screen shows an agent CLI
// waiting on the user.
func detectAgentPrompt(rows []string) bool {
	var nonBlank []string
	for _, r := range rows {
		if strings.TrimSpace(strings.Trim(r, "│|╭╮╰╯─ ")) != "" {
			nonBlank = append(nonBlank, r)
		}
	}
	if len(nonBlank) == 0 {
		return false
	}
	window := lastN(nonBlank, attentionWindow)
	tail := lastN(nonBlank, attentionTail)
	for _, r := range window {
		if strings.Contains(r, "esc to i") { // still working (ansaScreenIsWorking's marker)
			return false
		}
	}
	for _, r := range tail {
		if strings.Contains(r, attentionMenuFooter) {
			return true
		}
	}
	question := false
	for _, r := range window {
		for _, q := range attentionQuestions {
			if strings.Contains(r, q) {
				question = true
			}
		}
	}
	if !question {
		return false
	}
	for _, r := range tail {
		if attentionOptionYes.MatchString(r) {
			return true
		}
		for _, a := range attentionAnswers {
			if strings.Contains(r, a) {
				return true
			}
		}
	}
	return false
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// attentionInput is one look at a pane, for attentionTracker.step.
type attentionInput struct {
	now        time.Time
	lastOutput time.Time // last PTY output
	lastInput  time.Time // last keystroke written to the PTY
	program    string    // foreground program; "" when the shell has the terminal
	rows       func() []string
}

// attentionTracker decides the prompt state of one pane. Pure: the watcher
// goroutine feeds it timestamps and a screen reader, and it is tested without
// a PTY or a clock.
type attentionTracker struct {
	active     bool
	raisedAt   time.Time
	lastRead   time.Time // lastOutput at the last screen read
	answeredAt time.Time // input that cleared the last prompt
}

// step returns whether the state changed, and the state.
func (t *attentionTracker) step(in attentionInput) (changed, active bool) {
	set := func(v bool) (bool, bool) {
		if v == t.active {
			return false, v
		}
		t.active = v
		if v {
			t.raisedAt = in.now
		}
		return true, v
	}
	if in.program == "" {
		return set(false)
	}
	// The user typed into the pane after it was raised: they are on it.
	if t.active && in.lastInput.After(t.raisedAt) {
		t.answeredAt = in.lastInput
		return set(false)
	}
	// Read the screen only once it is quiet, and only when it changed since
	// the last read.
	if in.lastOutput.IsZero() || in.now.Sub(in.lastOutput) < attentionQuiet || !in.lastOutput.After(t.lastRead) {
		return false, t.active
	}
	t.lastRead = in.lastOutput
	// A screen the user already answered stays answered until new output.
	if !t.answeredAt.IsZero() && !in.lastOutput.After(t.answeredAt) {
		return set(false)
	}
	return set(detectAgentPrompt(in.rows()))
}

// paneAttentionMsg carries the watcher's verdict onto the pane's mailbox.
type paneAttentionMsg struct{ active bool }

// paneAttention is the pane's attention state. Owned by the actor goroutine.
type paneAttention struct {
	prompt      bool
	promptSince int64
	// The held native request, if any: it outranks a prompt.
	nativeID     string
	nativeReason string
	nativeSince  int64
}

// effective returns the reason and since of the pane's current attention.
func (a *paneAttention) effective() (reason string, since int64) {
	if a.nativeID != "" {
		return a.nativeReason, a.nativeSince
	}
	if a.prompt {
		return domain.AttentionReasonPrompt, a.promptSince
	}
	return "", 0
}

// applyPromptAttention records the watcher's verdict.
func (p *PaneActor) applyPromptAttention(active bool) {
	if p.attn.prompt == active {
		return
	}
	before, _ := p.attn.effective()
	p.attn.prompt = active
	if active {
		p.attn.promptSince = time.Now().UnixMilli()
	}
	p.attentionChanged(before)
}

// noteApprovalRequest records a held tool call or question.
func (p *PaneActor) noteApprovalRequest(m *msg.MsgApprovalRequest) {
	if m == nil || m.RequestID == "" {
		return
	}
	before, _ := p.attn.effective()
	p.attn.nativeID = m.RequestID
	p.attn.nativeReason = domain.AttentionReasonApproval
	if m.Type == msg.ApprovalTypeQuestion {
		p.attn.nativeReason = domain.AttentionReasonQuestion
	}
	p.attn.nativeSince = time.Now().UnixMilli()
	p.attentionChanged(before)
}

// noteApprovalResponse clears the held request it answers, and nothing else.
func (p *PaneActor) noteApprovalResponse(m *msg.MsgApprovalResponse) {
	if m == nil || m.RequestID == "" || m.RequestID != p.attn.nativeID {
		return
	}
	before, _ := p.attn.effective()
	p.attn.nativeID, p.attn.nativeReason, p.attn.nativeSince = "", "", 0
	p.attentionChanged(before)
}

// attentionChanged tells every frontend about a change of the effective state.
// A prompt is announced on ws.attention, where the TUI lights the pane; its
// clearing sends an ack. Native approvals keep their own, existing event
// (gated by `##attention`), so they are not announced twice. Every change
// fires one layoutDirty so snapshot readers (web, pane list) see it now.
func (p *PaneActor) attentionChanged(before string) {
	after, _ := p.attn.effective()
	if after == before {
		return
	}
	p.noteAttentionEdge(before, after)
	if p.pub == nil {
		return
	}
	switch {
	case after == domain.AttentionReasonPrompt:
		_ = p.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionEvent{
			PaneID:    p.id,
			Category:  msg.AttentionPrompt,
			Priority:  msg.AttentionPriorityHigh,
			Title:     "Needs you",
			Timestamp: time.Now().Unix(),
		})
	case before == domain.AttentionReasonPrompt && after == "":
		_ = p.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionAck{PaneID: p.id})
	}
	p.notifyLayoutDirty()
}

// withAttentionMeta returns meta plus the attention keys, for a live snapshot.
// It never modifies meta, and it adds nothing when the pane does not need you.
func (p *PaneActor) withAttentionMeta(meta map[string]string) map[string]string {
	reason, since := p.attn.effective()
	if reason == "" {
		return meta
	}
	out := make(map[string]string, len(meta)+3)
	for k, v := range meta {
		out[k] = v
	}
	out[domain.PaneMetaAttention] = domain.AttentionNeedsYou
	out[domain.PaneMetaAttentionReason] = reason
	out[domain.PaneMetaAttentionSince] = strconv.FormatInt(since, 10)
	return out
}

// withoutAttentionMeta drops attention keys from restored meta.
func withoutAttentionMeta(meta map[string]string) map[string]string {
	for k := range meta {
		if domain.IsAttentionMetaKey(k) {
			delete(meta, k)
		}
	}
	return meta
}

// snapshotMeta is the meta a snapshot carries: live snapshots get the
// attention keys, the persisted one (includeConversations) never does.
func (p *PaneActor) snapshotMeta(persisted bool) map[string]string {
	if persisted {
		return p.metaCopy()
	}
	return p.withAttentionMeta(p.metaCopy())
}

// screenReader is the part of the VTerm the watcher needs.
type screenReader interface{ Render() []string }

// watchAttention runs for as long as the pane's read loop (stop closes with
// it). It only looks at timestamps on each tick and reads the screen when
// attentionTracker.step asks; it never touches the PTY.
func (p *PaneActor) watchAttention(vt screenReader, stop <-chan struct{}) {
	if vt == nil {
		return
	}
	send := func(active bool) {
		if f, ok := p.attentionSend.Load().(func(bool)); ok && f != nil {
			f(active)
		}
	}
	var tr attentionTracker
	tick := time.NewTicker(attentionTick)
	defer tick.Stop()
	nanos := func(n int64) time.Time {
		if n == 0 {
			return time.Time{}
		}
		return time.Unix(0, n)
	}
	for {
		select {
		case <-stop:
			if tr.active {
				send(false)
			}
			return
		case now := <-tick.C:
			changed, active := tr.step(attentionInput{
				now:        now,
				lastOutput: nanos(p.lastOutputNano.Load()),
				lastInput:  nanos(p.lastInputNano.Load()),
				program:    p.Program(),
				rows:       vt.Render,
			})
			if changed {
				send(active)
			}
		}
	}
}
