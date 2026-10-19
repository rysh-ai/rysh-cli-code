// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
)

const (
	agentsTestRoom  = "7c1d2e3f-aaaa-4bbb-8ccc-dddddddddddd"
	agentsTestOwner = "0b6f3c5e-1111-4222-8333-944455556666"
	agentsTestMeet  = "https://meet.google.com/abc-defg-hij"
)

type agentsRoom struct {
	events chan meeting.LiveKitEvent
	once   sync.Once
}

func (r *agentsRoom) Events() <-chan meeting.LiveKitEvent { return r.events }
func (r *agentsRoom) Play(context.Context, []byte) error  { return nil }
func (r *agentsRoom) Close() error                        { r.once.Do(func() { close(r.events) }); return nil }

// meetingAgentsRig is a bare WorkspaceActor with a registry, config-tier
// secrets and variables, a fake rysh-server and fake LiveKit room, and the
// NATS-facing seams recorded.
type meetingAgentsRig struct {
	w       *WorkspaceActor
	server  string
	mu      sync.Mutex
	meta    map[string]map[string]string // pane → key → value
	notices chan string
	tokens  []string // agent names asked for at /agent-token
}

func newMeetingAgentsRig(t *testing.T, vars map[string]string) *meetingAgentsRig {
	t.Chdir(t.TempDir())
	for _, k := range []string{"RECALL_API_KEY", "RECALL_VERIFICATION_SECRET", "OPENAI_API_KEY", "DEEPGRAM_API_KEY", meeting.RyshAPIKeySecret, MeetingPublicURLVar, MeetingRecallRegionVar, MeetingAllowVar} {
		t.Setenv(k, "")
	}
	h := &meetingAgentsRig{meta: map[string]map[string]string{}, notices: make(chan string, 16)}
	h.w = &WorkspaceActor{meeting: meeting.NewManager()}
	t.Cleanup(h.w.meeting.Close)
	scope, _ := h.w.secretWorkspaceScope()
	h.w.secrets = newSecretStore(nil, map[string]map[string]string{scope: {"OPENAI_API_KEY": "speech", meeting.RyshAPIKeySecret: "rysh_testkey", "DEEPGRAM_API_KEY": "dg"}})
	h.w.variables = newVariableStore(nil, map[string]map[string]string{scope: vars})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rysh_testkey" {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/meeting-rooms/" + agentsTestRoom:
			_ = json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": agentsTestRoom, "owner_id": agentsTestOwner, "room_name": "rysh-existing"}})
		case "POST /api/meeting-rooms/" + agentsTestRoom + "/agent-token":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			h.mu.Lock()
			h.tokens = append(h.tokens, b["agent"])
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "agent-jwt", "url": "wss://lk.example", "room_name": "rysh-existing", "identity": "agent-" + b["agent"]})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	h.server = srv.URL

	oldDial, oldRes, oldExec, oldNotice, oldMeta, oldWatch := meeting.DialLiveKit, meetingAgentResolver, meetingSessionExec, meetingPaneNotice, meetingPaneMeta, meetingBotWatch
	t.Cleanup(func() {
		meeting.DialLiveKit, meetingAgentResolver, meetingSessionExec, meetingPaneNotice, meetingPaneMeta, meetingBotWatch = oldDial, oldRes, oldExec, oldNotice, oldMeta, oldWatch
	})
	meeting.DialLiveKit = func(context.Context, string, string, meeting.LiveKitDialOptions) (meeting.LiveKitRoom, error) {
		return &agentsRoom{events: make(chan meeting.LiveKitEvent)}, nil
	}
	noop := func(context.Context, string) (string, error) { return "", nil }
	meetingAgentResolver = func(_ *WorkspaceActor, name string) (meeting.Agent, error) {
		if err := meeting.ValidAgentName(name); err != nil {
			return meeting.Agent{}, err
		}
		return meeting.Agent{Name: name, Execute: noop}, nil
	}
	meetingSessionExec = func(*WorkspaceActor) (meeting.Execute, error) { return noop, nil }
	meetingPaneNotice = func(_ *WorkspaceActor, paneID, text string) { h.notices <- paneID + ":" + text }
	meetingPaneMeta = func(_ *WorkspaceActor, paneID, key, value string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.meta[paneID] == nil {
			h.meta[paneID] = map[string]string{}
		}
		h.meta[paneID][key] = value
	}
	meetingBotWatch = 10 * time.Millisecond
	return h
}

func (h *meetingAgentsRig) register(t *testing.T, c meeting.Command) string {
	t.Helper()
	c.Action = "register"
	r := h.w.meeting.Do(c, meeting.Credentials{}, nil)
	if r.Error != "" {
		t.Fatalf("register %+v", r)
	}
	return r.ID
}

// run issues one `##meeting …` agent subcommand and waits for its result.
func (h *meetingAgentsRig) run(t *testing.T, args ...string) string {
	t.Helper()
	var out strings.Builder
	h.w.ryshFail = nil
	h.w.meetingAgentCommand(&out, "p1", args)
	if h.w.ryshFail != nil {
		return "FAIL " + out.String()
	}
	select {
	case n := <-h.notices:
		if !strings.HasPrefix(n, "p1:") {
			t.Fatalf("notice went to the wrong pane: %q", n)
		}
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no result")
	}
	return ""
}

func (h *meetingAgentsRig) metaOf(pane, key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.meta[pane][key]
}

func TestMeetingAddAgentLiveKitFlowUpdatesPaneMeta(t *testing.T) {
	h := newMeetingAgentsRig(t, nil)
	id := h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})

	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "agents scribe") {
		t.Fatalf("first add: %q", got)
	}
	if len(h.tokens) != 1 || h.tokens[0] != "scribe" {
		t.Fatalf("agent-token for %v", h.tokens)
	}
	if h.metaOf("mp", "meeting.agents") != "scribe" {
		t.Fatalf("meta %v", h.meta)
	}
	// The join watcher moves meeting.bot from joining to in_call (the bot polls every 5 s).
	for deadline := time.Now().Add(15 * time.Second); h.metaOf("mp", "meeting.bot") != "in_call"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("bot phase %q", h.metaOf("mp", "meeting.bot"))
		}
	}
	// The second agent is its own participant (design 043 D9): its own
	// agent token, and each bot's phase in meeting.bots.
	if got := h.run(t, "add", "agent", id, "coder"); !strings.Contains(got, "agents scribe, coder") || strings.Join(h.tokens, ",") != "scribe,coder" {
		t.Fatalf("second add: %q tokens %v", got, h.tokens)
	}
	if h.metaOf("mp", "meeting.agents") != "scribe,coder" || !strings.HasPrefix(h.metaOf("mp", "meeting.bots"), "scribe:in_call,coder:") {
		t.Fatalf("meta %v", h.meta)
	}
	for deadline := time.Now().Add(15 * time.Second); h.metaOf("mp", "meeting.bots") != "scribe:in_call,coder:in_call"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("bots %q", h.metaOf("mp", "meeting.bots"))
		}
	}
	if got := h.run(t, "remove", "agent", id, "coder"); !strings.Contains(got, "agents scribe") {
		t.Fatalf("remove: %q", got)
	}
	if got := h.run(t, "remove", "agent", id, "scribe"); !strings.Contains(got, "no agents left") || !strings.Contains(got, "bot left") {
		t.Fatalf("last remove: %q", got)
	}
	if h.metaOf("mp", "meeting.agents") != "" || h.metaOf("mp", "meeting.bot") != "left" || h.metaOf("mp", "meeting.bots") != "" {
		t.Fatalf("meta after last remove %v", h.meta)
	}
	if _, ok := h.w.meeting.Lookup(id); !ok {
		t.Fatal("the meeting was dropped with its bot")
	}
	if got := h.run(t, "remove", "agent", id, "scribe"); !strings.Contains(got, "has no bot") || h.metaOf("mp", "meeting.bot") != "left" {
		t.Fatalf("remove from a bot-less meeting: %q meta %v", got, h.meta)
	}
}

func TestMeetingAddAgentWebUsesJoinRulesAndPublicURLVar(t *testing.T) {
	h := newMeetingAgentsRig(t, nil)
	id := h.register(t, meeting.Command{PaneID: "mp", URL: agentsTestMeet})
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "--public-url") {
		t.Fatalf("no public URL: %q", got)
	}
	if got := h.run(t, "add", "agent", id, "scribe", "--public-url", "https://pub.example"); !strings.Contains(got, "--consent") {
		t.Fatalf("no consent: %q", got)
	}
	// With the session variable, the public URL passes and the missing
	// Recall key is next.
	h = newMeetingAgentsRig(t, map[string]string{MeetingPublicURLVar: "https://pub.example"})
	id = h.register(t, meeting.Command{PaneID: "mp", URL: agentsTestMeet})
	if got := h.run(t, "add", "agent", id, "scribe", "--consent"); !strings.Contains(got, "RECALL_API_KEY") {
		t.Fatalf("var default: %q", got)
	}
	if len(h.meta) != 0 {
		t.Fatalf("a refused add wrote pane meta: %v", h.meta)
	}
	// MEETING_RECALL_REGION reaches the join (a bad one is refused by
	// 040's region check before the keys are looked at).
	h = newMeetingAgentsRig(t, map[string]string{MeetingPublicURLVar: "https://pub.example", MeetingRecallRegionVar: "mars-1"})
	id = h.register(t, meeting.Command{PaneID: "mp", URL: agentsTestMeet})
	if got := h.run(t, "add", "agent", id, "scribe", "--consent"); !strings.Contains(got, "unsupported Recall region") {
		t.Fatalf("region var: %q", got)
	}
	h = newMeetingAgentsRig(t, map[string]string{MeetingPublicURLVar: "https://pub.example", MeetingRecallRegionVar: " eu-central-1 "})
	id = h.register(t, meeting.Command{PaneID: "mp", URL: agentsTestMeet})
	if got := h.run(t, "add", "agent", id, "scribe", "--consent"); !strings.Contains(got, "RECALL_API_KEY") {
		t.Fatalf("valid region var: %q", got)
	}
}

func TestMeetingAgentCommandRefusals(t *testing.T) {
	h := newMeetingAgentsRig(t, nil)
	id := h.register(t, meeting.Command{PaneID: "mp", URL: agentsTestMeet})
	for _, args := range [][]string{
		{"add", "agent", id},
		{"add", "bot", id, "scribe"},
		{"add", "agent", id, "scribe", "--bogus"},
		{"remove", "agent", id, "scribe", "--consent"},
		{"add", "agent", "meeting-1", "scribe"},
		{"add", "agent", "m-ffffff", "scribe"},
		{"add", "agent", id, "rysh"},
	} {
		if got := h.run(t, args...); !strings.HasPrefix(got, "FAIL") {
			t.Errorf("%v accepted: %q", args, got)
		}
	}
	h.w.meeting = nil
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "no meeting") {
		t.Fatalf("no registry: %q", got)
	}
}

func TestMeetingBotPhase(t *testing.T) {
	for state, want := range map[string]string{"": "", "idle": "", "joining": "joining", "create_uncertain": "joining", "starting": "joining", "listening": "in_call", "speaking": "in_call", "stopped": "left", "create_failed": "failed", "leave_failed": "failed"} {
		if got := meetingBotPhase(meeting.Reply{State: state}); got != want {
			t.Errorf("%q → %q, want %q", state, got, want)
		}
	}
}

func (h *meetingAgentsRig) allowOf(t *testing.T, id string) string {
	t.Helper()
	r := h.w.meeting.Do(meeting.Command{Action: "status", ID: id}, meeting.Credentials{}, nil)
	return strings.Join(r.Allow, "|")
}

func TestMeetingAddAgentAllow(t *testing.T) {
	a, err := parseMeetingAgentArgs([]string{"add", "agent", "m-abc123", "scribe", "--allow", "host, Halil Ağın"})
	if err != nil || !a.allowFromCmd || strings.Join(a.allow, "|") != "host|Halil Ağın" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := parseMeetingAgentArgs([]string{"add", "agent", "m-abc123", "scribe", "--allow=anyone"}); err != nil || strings.Join(a.allow, "|") != "anyone" {
		t.Fatalf("%+v %v", a, err)
	}
	// ## input arrives split on spaces, quotes included.
	for _, args := range [][]string{
		{"add", "agent", "m-abc123", "scribe", "--allow", `"host,Halil`, `Ağın"`, "--consent"},
		{"add", "agent", "m-abc123", "scribe", "--consent", "--allow", "host,Halil", "Ağın"},
		{"add", "agent", "m-abc123", "scribe", `--allow='host,Halil`, `Ağın'`},
	} {
		a, err := parseMeetingAgentArgs(args)
		if err != nil || strings.Join(a.allow, "|") != "host|Halil Ağın" || (args[len(args)-1] == "--consent" || args[4] == "--consent") != a.consent {
			t.Errorf("%q: %+v %v", args, a, err)
		}
	}
	if _, err := parseMeetingAgentArgs([]string{"remove", "agent", "m-abc123", "scribe", "--allow", "host"}); err == nil {
		t.Fatal("remove accepted --allow")
	}

	// The flag reaches the bot; on an attach it is refused.
	h := newMeetingAgentsRig(t, map[string]string{MeetingAllowVar: "host,Var Name"})
	id := h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe", "--allow", "anyone"); !strings.Contains(got, "agents scribe") || h.allowOf(t, id) != "anyone" {
		t.Fatalf("flag: %q allow %q", got, h.allowOf(t, id))
	}
	if got := h.run(t, "add", "agent", id, "coder", "--allow", "host"); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "allowlist was fixed") {
		t.Fatalf("attach with --allow: %q", got)
	}
	// Without the flag the variable is not applied to an attach.
	if got := h.run(t, "add", "agent", id, "coder"); !strings.Contains(got, "agents scribe, coder") || h.allowOf(t, id) != "anyone" {
		t.Fatalf("attach: %q allow %q", got, h.allowOf(t, id))
	}

	// MEETING_ALLOW is the default at bot start; a bad value is refused.
	h = newMeetingAgentsRig(t, map[string]string{MeetingAllowVar: "host, Var Name"})
	id = h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "agents scribe") || h.allowOf(t, id) != "host|Var Name" {
		t.Fatalf("var: %q allow %q", got, h.allowOf(t, id))
	}
	h = newMeetingAgentsRig(t, nil)
	id = h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe", "--allow", "anyone,host"); !strings.Contains(got, "cannot be combined") || len(h.tokens) != 0 {
		t.Fatalf("bad: %q tokens %v", got, h.tokens)
	}
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "agents scribe") || h.allowOf(t, id) != "host" {
		t.Fatalf("default: %q allow %q", got, h.allowOf(t, id))
	}
}

// Design 048: --tts/--stt on add agent, else MEETING_TTS / MEETING_STT,
// else Deepgram; the chosen provider's key reaches the bot.
func TestMeetingAddAgentSpeechProviders(t *testing.T) {
	for _, args := range [][]string{
		{"add", "agent", "m-abc123", "scribe", "--tts", "google"},
		{"add", "agent", "m-abc123", "scribe", "--stt=openai"},
		{"remove", "agent", "m-abc123", "scribe", "--tts", "elevenlabs"},
	} {
		if _, err := parseMeetingAgentArgs(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	a, err := parseMeetingAgentArgs([]string{"add", "agent", "m-abc123", "scribe", "--tts=elevenlabs", "--stt", "elevenlabs", "--consent"})
	if err != nil || a.tts != "elevenlabs" || a.stt != "elevenlabs" || !a.consent {
		t.Fatalf("%+v %v", a, err)
	}

	type dialed struct{ stt, key, lang string }
	rig := func(t *testing.T, vars map[string]string) (*meetingAgentsRig, chan dialed) {
		t.Setenv("ELEVENLABS_API_KEY", "")
		h := newMeetingAgentsRig(t, vars)
		scope, _ := h.w.secretWorkspaceScope()
		h.w.secrets = newSecretStore(nil, map[string]map[string]string{scope: {"OPENAI_API_KEY": "speech", meeting.RyshAPIKeySecret: "rysh_testkey", "DEEPGRAM_API_KEY": "dg", "ELEVENLABS_API_KEY": "el"}})
		got := make(chan dialed, 4)
		meeting.DialLiveKit = func(_ context.Context, _, _ string, o meeting.LiveKitDialOptions) (meeting.LiveKitRoom, error) {
			got <- dialed{o.STT, o.STTKey, o.Lang}
			return &agentsRoom{events: make(chan meeting.LiveKitEvent)}, nil
		}
		return h, got
	}
	wait := func(t *testing.T, ch chan dialed) dialed {
		t.Helper()
		select {
		case d := <-ch:
			return d
		case <-time.After(10 * time.Second):
			t.Fatal("bot never dialed")
		}
		return dialed{}
	}
	voice := func(h *meetingAgentsRig, id string) string {
		r := h.w.meeting.Do(meeting.Command{Action: "status", ID: id}, meeting.Credentials{}, nil)
		return strings.Join(r.Voices, ",")
	}

	// Default: Deepgram hears and speaks.
	h, dial := rig(t, nil)
	id := h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "agents scribe") {
		t.Fatalf("add: %q", got)
	}
	if d := wait(t, dial); d != (dialed{"deepgram", "dg", ""}) {
		t.Fatalf("dialed %+v", d)
	}
	if v := voice(h, id); !strings.HasPrefix(v, "aura-2-") {
		t.Fatalf("voice %q", v)
	}

	// The session variables choose ElevenLabs for both.
	h, dial = rig(t, map[string]string{MeetingTTSVar: " ElevenLabs ", MeetingSTTVar: "elevenlabs"})
	id = h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "agents scribe") {
		t.Fatalf("add: %q", got)
	}
	if d := wait(t, dial); d != (dialed{"elevenlabs", "el", ""}) {
		t.Fatalf("dialed %+v", d)
	}
	if v := voice(h, id); len(v) != 20 {
		t.Fatalf("voice %q, want an ElevenLabs voice id", v)
	}
	// A flag beats the variable.
	if got := h.run(t, "add", "agent", id, "coder", "--stt", "deepgram", "--tts", "openai"); !strings.Contains(got, "agents scribe, coder") {
		t.Fatalf("add: %q", got)
	}
	if d := wait(t, dial); d != (dialed{"deepgram", "dg", ""}) {
		t.Fatalf("dialed %+v", d)
	}

	// A bad variable is reported like a bad flag.
	h, _ = rig(t, map[string]string{MeetingSTTVar: "whisper"})
	id = h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "MEETING_STT") {
		t.Fatalf("bad var: %q", got)
	}

	// MEETING_LANGUAGE (or --lang) reaches the room's STT; Kurmanji with
	// Deepgram listening is refused, naming the fix.
	h, dial = rig(t, map[string]string{MeetingTTSVar: "elevenlabs", MeetingSTTVar: "elevenlabs", MeetingLanguageVar: "KMR"})
	id = h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})
	if got := h.run(t, "add", "agent", id, "scribe"); !strings.Contains(got, "agents scribe") {
		t.Fatalf("add: %q", got)
	}
	if d := wait(t, dial); d != (dialed{"elevenlabs", "el", "kmr"}) {
		t.Fatalf("dialed %+v", d)
	}
	if got := h.run(t, "add", "agent", id, "coder", "--stt", "deepgram"); !strings.Contains(got, "Deepgram does not transcribe Kurdish") {
		t.Fatalf("kurdish on deepgram: %q", got)
	}
	if _, err := parseMeetingAgentArgs([]string{"add", "agent", "m-abc123", "scribe", "--lang", "kurmanji"}); err == nil {
		t.Fatal("accepted --lang kurmanji")
	}
	if a, err := parseMeetingAgentArgs([]string{"add", "agent", "m-abc123", "scribe", "--lang=KMR"}); err != nil || a.lang != "kmr" {
		t.Fatalf("%+v %v", a, err)
	}
}
