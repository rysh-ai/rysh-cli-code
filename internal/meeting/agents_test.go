// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// recorder is a fake executor per agent: it records who answered what.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) exec(who string) Execute {
	return func(_ context.Context, text string) (string, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, who+":"+text)
		return who + " answers " + text, nil
	}
}
func (r *recorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[len(r.calls)-1]
}

func attachAll(t *testing.T, s *Service, agents ...Agent) {
	t.Helper()
	for _, a := range agents {
		if r := s.attach(a); r.Error != "" {
			t.Fatalf("attach %s: %s", a.Name, r.Error)
		}
	}
}

func TestMeetingWakeRoutesToAddressedAgent(t *testing.T) {
	for phrase, want := range map[string][2]string{
		"hey scribe summarise the call":          {"scribe", "summarise the call"},
		"Hey, Scribe. What was decided?":         {"scribe", "What was decided?"},
		"okay hey code reviewer check the diff":  {"code-reviewer", "check the diff"},
		"hi codereviewer check the diff":         {"code-reviewer", "check the diff"},
		"Scribe, list the action items":          {"scribe", "list the action items"},
		"hey code check the build":               {"code", "check the build"},
		"hey rysh what time is it":               {"", "what time is it"},
		"hey scribe then hey rysh which one":     {"scribe", "then hey rysh which one"},
		"so hey rish, and hey scribe, who wins?": {"", "and hey scribe, who wins?"},
	} {
		rec := &recorder{}
		s, _, _ := fixture(t, rec.exec("rysh"))
		attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")}, Agent{Name: "code-reviewer", Execute: rec.exec("cr")}, Agent{Name: "code", Execute: rec.exec("code")})
		deliver(t, s, "wake", transcript(phrase, 1, 1))
		r := s.status(true)
		if r.Agent != want[0] || r.Request != want[1] {
			t.Errorf("%q: agent %q request %q, want %q %q", phrase, r.Agent, r.Request, want[0], want[1])
		}
	}
	for _, phrase := range []string{"the scribe wrote it down", "hey there scribe", "we reviewed code"} {
		rec := &recorder{}
		s, _, _ := fixture(t, rec.exec("rysh"))
		attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")}, Agent{Name: "code-reviewer", Execute: rec.exec("cr")})
		deliver(t, s, "ambient", transcript(phrase, 1, 1))
		if s.status(false).TurnID != "" {
			t.Errorf("%q woke an agent", phrase)
		}
	}
}

func TestMeetingAgentGreetingSplitAcrossSegments(t *testing.T) {
	rec := &recorder{}
	s, _, _ := fixture(t, rec.exec("rysh"))
	attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")})
	deliver(t, s, "a", transcript("okay hey", 1, 1))
	deliver(t, s, "b", transcript("scribe what did we decide", 1, 3))
	if r := s.status(true); r.Agent != "scribe" || r.Request != "what did we decide" {
		t.Fatalf("split address: %+v", r)
	}
}

// The addressed agent's executor produces the reply and its voice speaks it;
// rysh and the other agents are never called.
func TestMeetingAgentTurnUsesAgentExecutorAndVoice(t *testing.T) {
	rec := &recorder{}
	s, conn, _ := fixture(t, rec.exec("rysh"))
	voices := make(chan string, 4)
	s.voice = func(_ context.Context, voice, text string) ([]byte, error) {
		voices <- voice
		return []byte("MP3:" + voice), nil
	}
	attachAll(t, s, Agent{Name: "scribe", Voice: "nova", Execute: rec.exec("scribe")}, Agent{Name: "planner", Execute: rec.exec("planner")})
	deliver(t, s, "wake", transcript("hey scribe list decisions", 1, 1))
	finalize(s)
	id := s.status(false).TurnID
	s.command(Command{Action: "approve", TurnID: id})
	waitFor(t, func() bool { return s.status(false).State == "awaiting_speech" })
	rec.mu.Lock()
	calls := len(rec.calls)
	rec.mu.Unlock()
	if got := rec.last(); got != "scribe:list decisions" || calls != 1 {
		t.Fatalf("executor calls %v", rec.calls)
	}
	if r := s.command(Command{Action: "pending"}); r.Result != "scribe answers list decisions" || r.Agent != "scribe" {
		t.Fatalf("pending %+v", r)
	}
	s.command(Command{Action: "speak", TurnID: id})
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var control map[string]string
	if err := conn.ReadJSON(&control); err != nil {
		t.Fatal(err)
	}
	kind, audio, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(audio) != "MP3:nova" {
		t.Fatalf("audio %q %v", audio, err)
	}
	if len(voices) != 1 || <-voices != "nova" {
		t.Fatal("agent voice not used exactly once")
	}
}

func TestMeetingPrimaryAgentAnswersHeyRysh(t *testing.T) {
	rec := &recorder{}
	s, _, _ := fixture(t, rec.exec("rysh"))
	attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")})
	s.mu.Lock()
	s.primary = "scribe"
	s.mu.Unlock()
	deliver(t, s, "wake", transcript("hey rysh take a note", 1, 1))
	if r := s.status(true); r.Agent != "scribe" || r.Request != "take a note" {
		t.Fatalf("hey rysh with primary: %+v", r)
	}
	s.detach("scribe")
	if r := s.status(false); r.TurnID != "" || r.State != "listening" || len(r.Agents) != 0 {
		t.Fatalf("detach kept the agent's turn: %+v", r)
	}
	s.mu.Lock()
	s.cooldown = time.Time{} // skip the post-clear echo cooldown
	s.mu.Unlock()
	deliver(t, s, "wake2", transcript("hey rysh take a note", 1, 5))
	if r := s.status(true); r.Agent != "" || r.Request != "take a note" {
		t.Fatalf("after detach hey rysh must reach rysh: %+v", r)
	}
}

// One of several agent bots (design 043 D9): "hey rysh" is nobody's — not the
// agent's and not rysh's — while "hey <its agent>" still wakes it.
func TestMeetingOneOfSeveralBotsIgnoresHeyRysh(t *testing.T) {
	rec := &recorder{}
	s, _, _ := fixture(t, rec.exec("rysh"))
	attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")})
	s.mu.Lock()
	s.primary = "scribe"
	s.mu.Unlock()
	s.setPrimary(false)
	deliver(t, s, "wake", transcript("hey rysh take a note", 1, 1))
	if r := s.status(true); r.TurnID != "" || r.State != "listening" || r.IgnoredWakes != 0 {
		t.Fatalf("hey rysh woke one of several bots: %+v", r)
	}
	deliver(t, s, "wake2", transcript("hey scribe take a note", 1, 5))
	if r := s.status(true); r.Agent != "scribe" || r.Request != "take a note" {
		t.Fatalf("hey scribe: %+v", r)
	}
}

// Detaching between capture and approval must not run anything, and a late
// result from a detached agent's executor is dropped.
func TestMeetingDetachedAgentNeverRunsOrSpeaks(t *testing.T) {
	rec := &recorder{}
	s, _, _ := fixture(t, rec.exec("rysh"))
	attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")})
	deliver(t, s, "wake", transcript("hey scribe do it", 1, 1))
	finalize(s)
	id := s.status(false).TurnID
	s.mu.Lock()
	s.agents = nil // detached without clearing: the executor lookup must fail closed
	s.mu.Unlock()
	s.command(Command{Action: "approve", TurnID: id})
	if r := s.status(false); r.State != "failed" || rec.last() != "" {
		t.Fatalf("detached agent ran: %+v %q", r, rec.last())
	}

	release := make(chan struct{})
	s2, _, _ := fixture(t, rec.exec("rysh"))
	attachAll(t, s2, Agent{Name: "scribe", Execute: func(ctx context.Context, _ string) (string, error) {
		<-release
		return "late", nil
	}})
	deliver(t, s2, "wake", transcript("hey scribe slow task", 1, 1))
	finalize(s2)
	s2.command(Command{Action: "approve", TurnID: s2.status(false).TurnID})
	waitFor(t, func() bool { return s2.status(false).State == "executing" })
	s2.detach("scribe")
	close(release)
	time.Sleep(50 * time.Millisecond)
	if r := s2.status(true); r.State != "listening" || r.Result != "" {
		t.Fatalf("late detached result kept: %+v", r)
	}
}

func TestBindAgentValidation(t *testing.T) {
	ok := func(context.Context, string) (string, error) { return "", nil }
	for _, name := range []string{"rysh", "rish", "hey-scribe", "agent2", "a", "x.y", "one-two-three-four", "harish"} {
		if _, err := bindAgent(Agent{Name: name, Execute: ok}, "openai", nil); err == nil {
			t.Errorf("accepted agent name %q", name)
		}
	}
	if _, err := bindAgent(Agent{Name: "scribe"}, "openai", nil); err == nil {
		t.Error("accepted agent without executor")
	}
	if _, err := bindAgent(Agent{Name: "scribe", Voice: "aura-2-apollo-en", Execute: ok}, "openai", nil); err == nil {
		t.Error("accepted Deepgram voice on OpenAI")
	}
	if _, err := bindAgent(Agent{Name: "scribe", Voice: "nova", Execute: ok}, "deepgram", nil); err == nil {
		t.Error("accepted OpenAI voice on Deepgram")
	}
	var attached []meetingAgent
	voices := map[string]bool{}
	for _, name := range []string{"scribe", "planner", "code_reviewer", "tester"} {
		b, err := bindAgent(Agent{Name: name, Execute: ok}, "openai", attached)
		if err != nil {
			t.Fatal(name, err)
		}
		if b.Voice == "" || voices[b.Voice] || b.Voice == "coral" {
			t.Errorf("%s voice %q not a distinct default", name, b.Voice)
		}
		voices[b.Voice] = true
		attached = append(attached, b)
	}
	if _, err := bindAgent(Agent{Name: "extra", Execute: ok}, "openai", attached); err == nil {
		t.Error("accepted a fifth agent")
	}
	if _, err := bindAgent(Agent{Name: "Scribe", Execute: ok}, "openai", attached[:1]); err == nil {
		t.Error("accepted a duplicate spoken name")
	}
}

func TestManagerAttachRequiresActiveMeeting(t *testing.T) {
	m := NewManager()
	ok := func(context.Context, string) (string, error) { return "", nil }
	if r := m.Do(Command{Action: "attach"}, Credentials{}, nil, Agent{Name: "scribe", Execute: ok}); r.Error == "" {
		t.Fatal("attached without a meeting")
	}
	if r := m.Do(Command{Action: "detach", Agent: "scribe"}, Credentials{}, nil); r.Error == "" {
		t.Fatal("detached without a meeting")
	}
}

// The founder's flow: hands-free, "hey scribe …" → the agent's executor runs →
// its reply is spoken in its voice, with no terminal input.
func TestMeetingHandsFreeAgentTurn(t *testing.T) {
	rec := &recorder{}
	s, conn := handsFree(t, rec.exec("rysh"))
	voices := make(chan string, 4)
	s.mu.Lock()
	s.voice = func(_ context.Context, voice, text string) ([]byte, error) {
		voices <- voice + "|" + text
		return []byte("MP3"), nil
	}
	s.mu.Unlock()
	attachAll(t, s, Agent{Name: "scribe", Voice: "nova", Execute: rec.exec("scribe")})
	deliver(t, s, "wake", said("hey scribe what did we decide", 1, "Host", true, 1))
	settle(s)
	turn, _ := readAudio(t, conn)
	if got := <-voices; got != "nova|scribe answers what did we decide" {
		t.Fatalf("spoke %q", got)
	}
	if rec.last() != "scribe:what did we decide" {
		t.Fatal("wrong executor:", rec.last())
	}
	_ = conn.WriteJSON(map[string]string{"type": "played", "turn": turn})
	waitFor(t, func() bool { return s.status(false).State == "listening" })

	// The speaker allowlist gates agents exactly as it gates rysh.
	s.mu.Lock()
	s.cooldown = time.Time{}
	s.mu.Unlock()
	deliver(t, s, "guest", said("hey scribe delete everything", 2, "Guest", false, 9))
	if r := s.status(false); r.TurnID != "" || r.IgnoredWakes != 1 {
		t.Fatal("non-host woke an agent:", r)
	}
}

// Hands-free, a turn for an agent detached before it ran says the failure
// phrase in rysh's voice instead of running anything.
func TestMeetingHandsFreeDetachedAgentFailsSpoken(t *testing.T) {
	rec := &recorder{}
	s, conn := handsFree(t, rec.exec("rysh"))
	var spoken atomic.Value
	s.mu.Lock()
	s.speech = func(_ context.Context, text string) ([]byte, error) { spoken.Store(text); return []byte("MP3"), nil }
	s.mu.Unlock()
	attachAll(t, s, Agent{Name: "scribe", Execute: rec.exec("scribe")})
	deliver(t, s, "wake", said("hey scribe do it", 1, "Host", true, 1))
	s.mu.Lock()
	s.agents = nil
	s.mu.Unlock()
	settle(s)
	readAudio(t, conn)
	if spoken.Load() != failurePhrase || rec.last() != "" {
		t.Fatalf("spoke %v, calls %q", spoken.Load(), rec.last())
	}
}

func TestMeetingAgentNameStopsCurrentTurn(t *testing.T) {
	agents := []meetingAgent{{Agent: Agent{Name: "scribe"}, words: []string{"scribe"}}, {Agent: Agent{Name: "code-reviewer"}, words: []string{"code", "reviewer"}}}
	for phrase, want := range map[string]bool{
		"scribe stop":                  true,
		"hey code reviewer, cancel":    true,
		"stop, scribe":                 true,
		"okay scribe that is enough":   false, // stop word too far from the name
		"scribe what should we stop":   false,
		"code stop":                    false,
		"please stop the scribe notes": true,
	} {
		if got := agentStopRequested(strings.Fields(phrase), agents); got != want {
			t.Errorf("%q: %v, want %v", phrase, got, want)
		}
	}
}

// status says which model answers the pending turn; attach explains a fallback.
func TestMeetingStatusNamesAnsweringModel(t *testing.T) {
	rec := &recorder{}
	s, _, _ := fixture(t, rec.exec("rysh"))
	s.mu.Lock()
	s.cfg.Model = "anthropic/sonnet"
	s.mu.Unlock()
	r := s.attach(Agent{Name: "scribe", Model: "anthropic/opus", Execute: rec.exec("scribe")})
	if !strings.Contains(r.Notice, "Model: anthropic/opus") {
		t.Fatal(r.Notice)
	}
	r = s.attach(Agent{Name: "planner", Model: "anthropic/sonnet", Note: "model gpt-9 is not configured in this session", Execute: rec.exec("planner")})
	if !strings.Contains(r.Notice, "gpt-9 is not configured") {
		t.Fatal(r.Notice)
	}
	deliver(t, s, "wake", transcript("hey scribe notes", 1, 1))
	if r := s.status(false); r.Model != "anthropic/opus" {
		t.Fatalf("agent turn model %q", r.Model)
	}
	s.command(Command{Action: "reject", TurnID: s.status(false).TurnID})
	s.mu.Lock()
	s.cooldown = time.Time{}
	s.mu.Unlock()
	deliver(t, s, "wake2", transcript("hey rysh notes", 1, 5))
	if r := s.status(false); r.Model != "anthropic/sonnet" || r.Agent != "" {
		t.Fatalf("rysh turn model %q", r.Model)
	}
}

// Founder decision 2026-09-28: an allowed speaker's "hey <agent>" runs the
// agent's tools with no approval; a non-allowed speaker is ignored; every
// call lands in the audit log and the status count; stop cancels the turn.
func TestMeetingAgentToolTurnAuditAndStop(t *testing.T) {
	audit := filepath.Join(t.TempDir(), "sess", "meeting-audit.jsonl")
	release := make(chan struct{})
	var runs atomic.Int32
	agentExec := func(ctx context.Context, text string) (string, error) {
		runs.Add(1)
		RecordToolCall(ctx, "bash", "git push --token=ghp_abcdefghijklmnop", "started")
		select {
		case <-release:
		case <-ctx.Done():
			RecordToolCall(ctx, "bash", "git push", "error")
			return "", ctx.Err()
		}
		RecordToolCall(ctx, "bash", "git push", "ok")
		return "Pushed.", nil
	}
	s, conn := handsFree(t, func(context.Context, string) (string, error) { return "rysh", nil })
	s.mu.Lock()
	s.cfg.AuditPath = audit
	s.mu.Unlock()
	attachAll(t, s, Agent{Name: "scribe", Execute: agentExec})

	deliver(t, s, "guest", said("hey scribe push the branch", 2, "Guest", false, 1))
	settle(s)
	if r := s.status(false); r.TurnID != "" || runs.Load() != 0 || r.IgnoredWakes != 1 {
		t.Fatal("non-allowed speaker ran an agent tool:", r)
	}

	deliver(t, s, "host", said("hey scribe push the branch", 1, "Host", true, 3))
	settle(s)
	waitFor(t, func() bool { return runs.Load() == 1 })
	close(release)
	turn, _ := readAudio(t, conn)
	_ = conn.WriteJSON(map[string]string{"type": "played", "turn": turn})
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	if r := s.status(false); r.ToolCalls != 1 || r.AuditLog != audit {
		t.Fatalf("status %+v", r)
	}

	// A second, long tool turn is stopped by "scribe stop".
	release = make(chan struct{})
	s.mu.Lock()
	s.cooldown = time.Time{}
	s.mu.Unlock()
	deliver(t, s, "host2", said("hey scribe push again", 1, "Host", true, 10))
	settle(s)
	waitFor(t, func() bool { return runs.Load() == 2 })
	deliver(t, s, "stop", said("scribe stop", 3, "Guest", false, 12))
	waitFor(t, func() bool { return s.status(false).ToolCalls == 2 })
	if r := s.status(false); r.TurnID != "" || r.State != "listening" {
		t.Fatal("stop did not end the tool turn:", r)
	}

	b, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 || strings.Contains(string(b), "ghp_abcdef") {
		t.Fatalf("audit log %s", b)
	}
	var e AuditEntry
	if json.Unmarshal([]byte(lines[1]), &e) != nil || e.Agent != "scribe" || e.Speaker != "Host" || e.Tool != "bash" || e.Status != "ok" || e.Meeting != botID || e.Turn == "" {
		t.Fatalf("entry %+v", e)
	}
	if json.Unmarshal([]byte(lines[3]), &e) != nil || e.Status != "error" {
		t.Fatalf("stopped entry %+v", e)
	}
	if info, _ := os.Stat(audit); info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode %v", info.Mode())
	}
}

func TestRedactToolTargets(t *testing.T) {
	for in, leak := range map[string]string{
		"curl -H 'Authorization: Bearer abcdefghijklmnop' https://x": "abcdefghijklmnop",
		"deploy --api-key=s3cr3tvalue":                               "s3cr3tvalue",
		"export GITHUB_TOKEN=ghp_1234567890abcdef":                   "ghp_1234",
		"git clone https://user:hunter22@example.com/r.git":          "hunter22",
		"echo aGVsbG8gd29ybGQgdGhpcyBpcyBhIGxvbmcgYmFzZTY0IGJsb2I=":  "aGVsbG8",
	} {
		if out := Redact(in); strings.Contains(out, leak) {
			t.Errorf("%q → %q leaks %q", in, out, leak)
		}
	}
	if got := Redact("go test ./internal/meeting"); got != "go test ./internal/meeting" {
		t.Errorf("over-redacted %q", got)
	}
}
