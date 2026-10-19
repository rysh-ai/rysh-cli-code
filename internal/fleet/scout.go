// SPDX-License-Identifier: Apache-2.0

package fleet

// The status scout (design 056 §3): a cheap model answers a status check-in on
// the agent's behalf, so the agent — on an expensive model, with a large
// context — is not woken to say "still working".
//
// WHY. A check-in is a full turn for the agent it lands on: its whole context
// is read again to produce one `fleet status` line. Most check-ins ask a
// question the outside can already answer from the agent's screen, its recent
// board posts and its ledger row. A small model reading those answers it; only
// when it cannot tell, or the message needs the agent to act, is the agent
// itself asked.
//
// WHAT THE SCOUT MAY ANSWER: status questions only — `silent`, `stalled`, and
// the `always` policy's routine check-in. Never `unblocked` (the agent must
// resume), never `unresponsive` (that is the agent's own silence), never the
// goal executor's asks (HURRY wants the agent's own estimate). And at most
// once per verdict: if the verdict stands after the scout answered — say it
// declared "progress" and the pane is still idle — the next check-in goes to
// the agent. A scout must never become a way for a stall to hide.
//
// Pure: the prompt, the parse and the decision. The model call is the actor's.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Who answers a check-in.
const (
	AnswerAgent = "agent" // the agent itself (default)
	AnswerScout = "scout" // a cheap model first; the agent only if it cannot tell
)

// DefaultScoutModel is the scout's model when none is set: the claude CLI's
// alias for the current Haiku.
const DefaultScoutModel = "haiku"

// ScoutMinConfidence is the least confidence at which a scout answer is used.
const ScoutMinConfidence = 0.7

// DeclaredByScout marks a declaration the scout made for an agent.
const DeclaredByScout = "scout"

// ScoutEligible reports whether the scout may answer this check-in.
func ScoutEligible(c Checkin, w Watch) bool {
	if w.Answer != AnswerScout || c.Member.Status == nil {
		return false
	}
	st := c.Member.Status
	switch c.Reason {
	case "scheduled":
		return true
	case HealthSilent, HealthStalled:
		// Once per verdict: the same verdict, already answered by the scout
		// since it began, goes to the agent.
		return !(st.ScoutAt > 0 && st.ScoutHealth == st.Health && st.ScoutAt >= st.HealthSince)
	}
	return false
}

// ScoutAnswer is the scout model's reply.
type ScoutAnswer struct {
	Answerable bool     `json:"answerable"`
	Work       string   `json:"work"`
	Task       string   `json:"task"`
	BlockedBy  []string `json:"blocked_by"`
	Note       string   `json:"note"`
	Confidence float64  `json:"confidence"`
	Reason     string   `json:"reason"`
}

// ScoutSystemPrompt is the scout's instruction.
const ScoutSystemPrompt = `You report the status of one software agent in a fleet, from the outside, so the agent itself is not interrupted.
You get its ledger row, the last lines of its terminal, its recent board posts, and whether a tool command is running in its pane.
Answer ONLY with one JSON object, no prose, no code fence:
{"answerable": true|false, "work": "idle|progress|blocked|done", "task": "<one line, or empty to keep>", "blocked_by": ["<member label>"|"human"|"ext:<what>"], "note": "<short evidence>", "confidence": 0.0-1.0, "reason": "<why>"}
Rules:
- answerable=false whenever the screen does not make the status clear, the agent seems to wait for a human or a decision you cannot name, it crashed or shows an error it must handle, or it asks a question. Being wrong is worse than asking.
- progress: it is visibly working on the task (a tool running, output being produced, a plan in progress).
- blocked: it says it waits on someone; name them in blocked_by using the member labels given, "human", or "ext:<what>".
- done: it reported the task finished. idle: it finished and has nothing to do.
- Never invent a task; leave task empty to keep the current one.`

// BuildScoutPrompt renders the evidence for one member.
func BuildScoutPrompt(f *Fleet, m *Member, reason string, screen []string, posts []string, toolRunning bool, now int64) string {
	var b strings.Builder
	st := m.Status
	if st == nil {
		st = &MemberStatus{}
	}
	fmt.Fprintf(&b, "Fleet %q. Member %s (role %s). Check-in reason: %s.\n", f.Name, MemberLabel(f, m.PaneID), orDash(m.Role), reason)
	if f.Goal != nil && f.Goal.Text != "" {
		fmt.Fprintf(&b, "Fleet goal: %s\n", f.Goal.Text)
	}
	if m.Goal != nil && m.Goal.Text != "" {
		fmt.Fprintf(&b, "Its goal: %s\n", m.Goal.Text)
	}
	fmt.Fprintf(&b, "Ledger: work=%s task=%q", orDash(st.Work), st.Task)
	if len(st.BlockedBy) > 0 {
		fmt.Fprintf(&b, " blocked_by=%s", labelsOf(f, st.BlockedBy))
	}
	if st.UpdatedAt > 0 {
		fmt.Fprintf(&b, " declared %s ago", DurationText(time.Duration(now-st.UpdatedAt)*time.Millisecond))
	}
	fmt.Fprintf(&b, "; observed activity=%s; tool command running now: %v\n", orDash(st.Activity), toolRunning)
	var labels []string
	for _, x := range f.Members {
		if x.PaneID != m.PaneID {
			labels = append(labels, MemberLabel(f, x.PaneID))
		}
	}
	fmt.Fprintf(&b, "Other members: %s\n", strings.Join(labels, ", "))
	if len(posts) > 0 {
		b.WriteString("Its recent board posts (oldest first):\n")
		for _, p := range posts {
			fmt.Fprintf(&b, "  - %s\n", clipText(p, 200))
		}
	}
	b.WriteString("Last lines of its terminal:\n-----\n")
	start := 0
	if len(screen) > 60 {
		start = len(screen) - 60
	}
	for _, l := range screen[start:] {
		b.WriteString(strings.TrimRight(l, " ") + "\n")
	}
	b.WriteString("-----\n")
	return b.String()
}

// ParseScoutAnswer reads the model's reply, tolerating a code fence or text
// around the JSON object.
func ParseScoutAnswer(text string) (ScoutAnswer, error) {
	i, j := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}')
	if i < 0 || j <= i {
		return ScoutAnswer{}, fmt.Errorf("no JSON object in the scout's reply")
	}
	var a ScoutAnswer
	if err := json.Unmarshal([]byte(text[i:j+1]), &a); err != nil {
		return ScoutAnswer{}, fmt.Errorf("unreadable scout reply: %w", err)
	}
	a.Work = strings.ToLower(strings.TrimSpace(a.Work))
	return a, nil
}

// ScoutUpdate turns a scout answer into a declaration, or says why it must
// not be used (and the agent asked instead).
func ScoutUpdate(a ScoutAnswer, model string) (StatusUpdate, error) {
	switch {
	case !a.Answerable:
		return StatusUpdate{}, fmt.Errorf("the scout could not tell: %s", orDash(a.Reason))
	case a.Confidence < ScoutMinConfidence:
		return StatusUpdate{}, fmt.Errorf("the scout was not sure enough (%.2f)", a.Confidence)
	case !ValidWork(a.Work):
		return StatusUpdate{}, fmt.Errorf("the scout answered an unknown work state %q", a.Work)
	case a.Work == WorkBlocked && len(a.BlockedBy) == 0:
		return StatusUpdate{}, fmt.Errorf("the scout said blocked without naming a blocker")
	}
	note := "answered by the status scout (" + model + ")"
	if s := strings.TrimSpace(a.Note); s != "" {
		note += ": " + s
	}
	u := StatusUpdate{Work: a.Work, Task: strings.TrimSpace(a.Task), Note: note, By: DeclaredByScout, KeepTask: true}
	if a.Work == WorkBlocked {
		u.BlockedBy = a.BlockedBy
	}
	return u, nil
}

// ScoutAnswered records that the scout answered this member's current verdict.
func (r *Registry) ScoutAnswered(fleetName, paneID, health string, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, m, err := r.memberLocked(fleetName, paneID); err == nil && m.Status != nil {
		m.Status.ScoutAt, m.Status.ScoutHealth = now, health
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
