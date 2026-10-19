// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func addAgent(m *Manager, id, name string, c Command, keys Credentials) Reply {
	c.Action, c.ID = "add_agent", id
	return m.Do(c, keys, noopExec, Agent{Name: name, Execute: noopExec})
}

func removeAgent(m *Manager, id, name string) Reply {
	return m.Do(Command{Action: "remove_agent", ID: id, Agent: name}, Credentials{}, nil)
}

// recallBodies records the bot-create bodies sent to the fake Recall.
func recallBodies(t *testing.T) *[]map[string]any {
	var mu sync.Mutex
	bodies := &[]map[string]any{}
	prev := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = prev })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/bot/" && strings.HasSuffix(r.URL.Hostname(), "recall.ai") {
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			*bodies = append(*bodies, b)
			mu.Unlock()
		}
		return prev.RoundTrip(r)
	})
	return bodies
}

func webAdd() Command {
	return Command{PublicURL: "https://meeting.example", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true}
}

// mediaURL is the media page URL a create body gives Recall.
func mediaURL(body map[string]any) string {
	cam, _ := body["output_media"].(map[string]any)["camera"].(map[string]any)
	cfg, _ := cam["config"].(map[string]any)
	u, _ := cfg["url"].(string)
	return u
}

// Design 043 D9 (founder, 2026-09-29): every agent is its own bot.
func TestAddAgentWebStartsOneRecallBotPerAgent(t *testing.T) {
	creates, leaves := fakeRecall(t)
	bodies := recallBodies(t)
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID

	r := addAgent(m, id, "scribe", webAdd(), recallKeys)
	if r.Error != "" || r.ID != id || r.PaneID != "p1" || creates.Load() != 1 || strings.Join(r.Agents, ",") != "scribe" {
		t.Fatalf("first add %+v creates %d", r, creates.Load())
	}
	if len(*bodies) != 1 || (*bodies)[0]["meeting_url"] != meetA || (*bodies)[0]["bot_name"] != botName+" — say hey scribe" ||
		!strings.HasPrefix(mediaURL((*bodies)[0]), "https://meeting.example/scribe/media#") {
		t.Fatalf("create body %v", *bodies)
	}
	// The second agent starts its own bot, served under its own path; the
	// meeting's consent carries over.
	second := webAdd()
	second.Consent = false
	r = addAgent(m, id, "coder", second, recallKeys)
	if r.Error != "" || creates.Load() != 2 || strings.Join(r.Agents, ",") != "scribe,coder" || len(r.Bots) != 2 {
		t.Fatalf("second add %+v creates %d", r, creates.Load())
	}
	if (*bodies)[1]["bot_name"] != botName+" — say hey coder" || !strings.HasPrefix(mediaURL((*bodies)[1]), "https://meeting.example/coder/media#") {
		t.Fatalf("second body %v", (*bodies)[1])
	}
	if r := addAgent(m, id, "coder", webAdd(), recallKeys); !strings.Contains(r.Error, "already in meeting") || creates.Load() != 2 {
		t.Fatalf("the same agent twice %+v", r)
	}

	r = removeAgent(m, id, "scribe")
	if r.Error != "" || strings.Join(r.Agents, ",") != "coder" || leaves.Load() != 1 || !strings.Contains(r.Notice, "scribe's bot left") {
		t.Fatalf("remove %+v leaves %d", r, leaves.Load())
	}
	if r := removeAgent(m, id, "nobody"); !strings.Contains(r.Error, "not in meeting") {
		t.Fatalf("%+v", r)
	}
	r = removeAgent(m, id, "coder")
	if r.Error != "" || r.State != "stopped" || leaves.Load() != 2 || !strings.Contains(r.Notice, "Last agent removed") {
		t.Fatalf("last remove %+v leaves %d", r, leaves.Load())
	}
	// The meeting (and its pane) stays, bot-less; a new add starts again.
	if mt, ok := m.Lookup(id); !ok || mt.hasBot() || mt.PaneID != "p1" {
		t.Fatalf("meeting after last remove %+v %v", mt, ok)
	}
	if r := removeAgent(m, id, "coder"); !strings.Contains(r.Error, "has no bot") {
		t.Fatalf("%+v", r)
	}
	if r := addAgent(m, id, "scribe", webAdd(), recallKeys); r.Error != "" || creates.Load() != 3 {
		t.Fatalf("re-add %+v", r)
	}
}

func TestAddAgentCapsBotsPerMeeting(t *testing.T) {
	creates, _ := fakeRecall(t)
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID
	for _, name := range []string{"ada", "bob", "cid", "dan"} {
		if r := addAgent(m, id, name, webAdd(), recallKeys); r.Error != "" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	r := addAgent(m, id, "eve", webAdd(), recallKeys)
	if !strings.Contains(r.Error, "4 agent bots, the limit") || creates.Load() != MaxAgentBots {
		t.Fatalf("fifth add %+v creates %d", r, creates.Load())
	}
	// Several Recall bots per session: another meeting gets its own (D4 gone).
	other := register(t, m, Command{PaneID: "p2", URL: meetB}).ID
	if r := addAgent(m, other, "eve", webAdd(), recallKeys); r.Error != "" || creates.Load() != MaxAgentBots+1 {
		t.Fatalf("other meeting %+v", r)
	}
}

// With several agent bots nobody answers "hey rysh"; the last one left does
// again (founder decision 2026-09-29).
func TestAddAgentHeyRyshOnlyWithOneBot(t *testing.T) {
	fakeRecall(t)
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID
	noRysh := func() []bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []bool
		for _, s := range m.meetings[id].Bots {
			s.mu.Lock()
			out = append(out, s.noRysh)
			s.mu.Unlock()
		}
		return out
	}
	addAgent(m, id, "scribe", webAdd(), recallKeys)
	if got := noRysh(); len(got) != 1 || got[0] {
		t.Fatalf("one bot: %v", got)
	}
	addAgent(m, id, "coder", webAdd(), recallKeys)
	if got := noRysh(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("two bots: %v", got)
	}
	removeAgent(m, id, "scribe")
	if got := noRysh(); len(got) != 1 || got[0] {
		t.Fatalf("back to one: %v", got)
	}
}

// Two bots share one listener and are told apart by path; the listener
// closes with its last bot.
func TestAgentBotsShareOneListener(t *testing.T) {
	fakeRecall(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID
	c := webAdd()
	c.Listen, c.PublicURL = addr, "https://meeting.example/meeting/mb"
	for _, name := range []string{"scribe", "coder"} {
		if r := addAgent(m, id, name, c, recallKeys); r.Error != "" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	get := func(path string) int {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for path, want := range map[string]int{"/meeting/mb/scribe/media": 200, "/meeting/mb/coder/media": 200, "/meeting/mb/media": 404, "/meeting/mb/nobody/media": 404} {
		if got := get(path); got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
	removeAgent(m, id, "scribe")
	if got := get("/meeting/mb/scribe/media"); got != 404 {
		t.Errorf("removed bot still served: %d", got)
	}
	removeAgent(m, id, "coder")
	listeners.Lock()
	_, open := listeners.byAddr[addr]
	listeners.Unlock()
	if open {
		t.Fatal("listener kept after its last bot left")
	}
}

func TestAddAgentWebFollows040JoinRules(t *testing.T) {
	creates, _ := fakeRecall(t)
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID
	noConsent := webAdd()
	noConsent.Consent = false
	noURL := webAdd()
	noURL.PublicURL = ""
	for name, tc := range map[string]struct {
		c    Command
		keys Credentials
	}{
		"consent":    {noConsent, recallKeys},
		"public url": {noURL, recallKeys},
		"keys":       {webAdd(), Credentials{Speech: "speech"}},
	} {
		if r := addAgent(m, id, "scribe", tc.c, tc.keys); r.Error == "" {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
	if creates.Load() != 0 {
		t.Fatalf("a bot was created: %d", creates.Load())
	}
	if mt, _ := m.Lookup(id); mt.hasBot() {
		t.Fatal("bot left behind")
	}
	if r := addAgent(m, "", "scribe", webAdd(), recallKeys); !strings.Contains(r.Error, "needs a meeting id") || !strings.Contains(r.Error, id) {
		t.Fatalf("id-less add %+v", r)
	}
	if r := addAgent(m, "m-ffffff", "scribe", webAdd(), recallKeys); !strings.Contains(r.Error, "no meeting m-ffffff") {
		t.Fatalf("%+v", r)
	}
}

func TestAddAgentLiveKitJoinsExistingRoom(t *testing.T) {
	h := newJoinRig(t)
	id := register(t, h.m, Command{PaneID: "p9", Room: existingRoom, Server: h.base}).ID
	keys := h.creds

	r := addAgent(h.m, id, "scribe", Command{}, keys)
	if r.Error != "" || r.ID != id || r.PaneID != "p9" || r.Room != "rysh-existing" || strings.Join(r.Agents, ",") != "scribe" {
		t.Fatalf("first add %+v", r)
	}
	if len(h.rs.agents) != 1 || h.rs.agents[0] != "scribe" || len(h.dialed) != 1 {
		t.Fatalf("agent-token %v dialed %v", h.rs.agents, h.dialed)
	}
	// The second agent is its own participant: its own agent token (D9).
	r = addAgent(h.m, id, "coder", Command{}, keys)
	if r.Error != "" || strings.Join(r.Agents, ",") != "scribe,coder" || strings.Join(h.rs.agents, ",") != "scribe,coder" || len(h.dialed) != 2 {
		t.Fatalf("second add %+v tokens %v dialed %v", r, h.rs.agents, h.dialed)
	}
	removeAgent(h.m, id, "scribe")
	r = removeAgent(h.m, id, "coder")
	if r.Error != "" || r.State != "stopped" {
		t.Fatalf("last remove %+v", r)
	}
	if mt, ok := h.m.Lookup(id); !ok || mt.hasBot() {
		t.Fatalf("meeting after last remove %+v %v", mt, ok)
	}
	for _, c := range h.rs.calls {
		if strings.HasPrefix(c, "DELETE") || strings.HasSuffix(c, "/meeting-rooms") {
			t.Fatalf("created or ended a room: %v", h.rs.calls)
		}
	}
}

func TestAddAgentLiveKitFailureLeavesMeetingBotless(t *testing.T) {
	h := newJoinRig(t)
	id := register(t, h.m, Command{PaneID: "p9", Room: existingRoom, Server: h.base}).ID
	h.rs.fail["POST /api/meeting-rooms/"+existingRoom+"/agent-token"] = 410
	r := addAgent(h.m, id, "scribe", Command{}, h.creds)
	if !strings.Contains(r.Error, "ended or expired") || r.ID != id {
		t.Fatalf("%+v", r)
	}
	if mt, ok := h.m.Lookup(id); !ok || mt.hasBot() {
		t.Fatalf("%+v %v", mt, ok)
	}
}

func TestAuditEntriesCarryMeetingID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meeting-audit.jsonl")
	s := newService(Command{ID: "m-abc123", AuditPath: path}, Credentials{}, noopExec)
	s.botID = botID
	ctx := s.withTurn(context.Background(), &turn{id: "t1", agent: "scribe", speaker: "Ann"})
	RecordToolCall(ctx, "bash", "ls", "ok")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	var e AuditEntry
	if err := json.Unmarshal(b, &e); err != nil || e.MeetingID != "m-abc123" || e.Meeting != botID || e.Agent != "scribe" {
		t.Fatalf("%s %+v %v", b, e, err)
	}
}

func TestRecallBotName(t *testing.T) {
	for _, tc := range []struct {
		agents []string
		want   string
	}{
		{nil, botName + " — say hey rysh"},
		{[]string{"scribe"}, botName + " — say hey scribe"},
		{[]string{"a", "b"}, botName + " — say hey rysh"},
		{[]string{strings.Repeat("x", 41)}, botName + " — say hey rysh"},
	} {
		if got := recallBotName(Command{Agents: tc.agents}); got != tc.want || !strings.HasPrefix(got, botName) {
			t.Errorf("%v → %q", tc.agents, got)
		}
	}
}

func TestAddAgentAllowlist(t *testing.T) {
	creates, _ := fakeRecall(t)
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID

	bad := webAdd()
	bad.Allow = []string{"anyone", "host"}
	if r := addAgent(m, id, "scribe", bad, recallKeys); !strings.Contains(r.Error, "cannot be combined") || creates.Load() != 0 {
		t.Fatalf("bad allow %+v", r)
	}
	c := webAdd()
	c.Allow, c.AllowFromFlag = []string{"host", "Halil Ağın"}, true
	r := addAgent(m, id, "scribe", c, recallKeys)
	if r.Error != "" || strings.Join(r.Allow, "|") != "host|Halil Ağın" {
		t.Fatalf("first add %+v", r)
	}
	// On a later agent an explicit --allow is refused, not ignored: the
	// allowlist belongs to the meeting, fixed by its first bot (D9).
	c = webAdd()
	c.Allow, c.AllowFromFlag = []string{"anyone"}, true
	if r := addAgent(m, id, "coder", c, recallKeys); !strings.Contains(r.Error, "allowlist was fixed") || creates.Load() != 1 {
		t.Fatalf("later add with --allow %+v", r)
	}
	if r := m.Do(Command{Action: "status", ID: id}, Credentials{}, nil); strings.Join(r.Agents, ",") != "scribe" || strings.Join(r.Allow, "|") != "host|Halil Ağın" {
		t.Fatalf("after refused add %+v", r)
	}
	// Without the flag (a variable default the daemon sets) the later bot
	// starts with the meeting's allowlist.
	c = webAdd()
	c.Allow = []string{"anyone"}
	r = addAgent(m, id, "coder", c, recallKeys)
	if r.Error != "" || creates.Load() != 2 || strings.Join(r.Allow, "|") != "host|Halil Ağın" {
		t.Fatalf("later add %+v", r)
	}
	for _, b := range r.Bots {
		if strings.Join(b.Allow, "|") != "host|Halil Ağın" {
			t.Fatalf("bot %v allow %v", b.Agents, b.Allow)
		}
	}
}

func TestAddAgentLiveKitAllowlist(t *testing.T) {
	h := newJoinRig(t)
	id := register(t, h.m, Command{PaneID: "p9", Room: existingRoom, Server: h.base}).ID
	if r := addAgent(h.m, id, "scribe", Command{Allow: []string{"anyone", "x"}}, h.creds); !strings.Contains(r.Error, "cannot be combined") || len(h.rs.calls) != 0 {
		t.Fatalf("bad allow %+v calls %v", r, h.rs.calls)
	}
	r := addAgent(h.m, id, "scribe", Command{Allow: []string{"guest-account"}, AllowFromFlag: true}, h.creds)
	if r.Error != "" || strings.Join(r.Allow, "|") != "guest-account" {
		t.Fatalf("%+v", r)
	}
}

// Every agent bot binds one agent; the meeting still gives each a voice of
// its own (the founder heard the difference in Run A and relies on it).
func TestAddAgentGivesEachBotItsOwnVoice(t *testing.T) {
	fakeRecall(t)
	m := NewManager()
	t.Cleanup(m.Close)
	id := register(t, m, Command{PaneID: "p1", URL: meetA}).ID
	for _, name := range []string{"scribe", "coder", "ada"} {
		if r := addAgent(m, id, name, webAdd(), recallKeys); r.Error != "" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	m.mu.Lock()
	bots := slices.Clone(m.meetings[id].Bots)
	m.mu.Unlock()
	var got []string
	for _, s := range bots {
		got = append(got, s.voices()...)
	}
	if strings.Join(got, ",") != strings.Join(defaultVoices[DefaultTTS][:3], ",") {
		t.Fatalf("voices %v", got)
	}
	// Status names each agent's voice, so a live run can tell (E-82).
	if r := statusOf(m, id); strings.Join(r.Voices, ",") != strings.Join(got, ",") || len(r.Bots) != 3 || r.Bots[1].Voices[0] != got[1] {
		t.Fatalf("status voices %+v", r)
	}
}

func statusOf(m *Manager, id string) Reply {
	return m.Do(Command{Action: "status", ID: id}, Credentials{}, nil)
}

func TestSpeechFailureNamesTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	if err := speechRequestError(ctx); err.Error() != "speech provider timed out" {
		t.Fatalf("%v", err)
	}
	if err := speechRequestError(context.Background()); err.Error() != "speech provider request failed" {
		t.Fatalf("%v", err)
	}
}

// Two agent bots in a rysh room speak in their own voices on the streamed
// path, as on Recall (the LiveKit bot used to drop per-agent voices).
func TestAddAgentLiveKitBotsStreamOwnVoices(t *testing.T) {
	h := newJoinRig(t)
	var mu sync.Mutex
	var voices []string
	prevWhole, prevStream := LiveKitVoiceSpeech, LiveKitVoiceSpeechStream
	t.Cleanup(func() { LiveKitVoiceSpeech, LiveKitVoiceSpeechStream = prevWhole, prevStream })
	LiveKitVoiceSpeech = func(string, string) func(context.Context, string, string) ([]byte, error) {
		return func(_ context.Context, voice, _ string) ([]byte, error) {
			mu.Lock()
			voices = append(voices, "whole:"+voice)
			mu.Unlock()
			return []byte("OggS"), nil
		}
	}
	LiveKitVoiceSpeechStream = func(string, string) func(context.Context, string, string) (io.ReadCloser, error) {
		return func(_ context.Context, voice, _ string) (io.ReadCloser, error) {
			mu.Lock()
			voices = append(voices, voice)
			mu.Unlock()
			return io.NopCloser(strings.NewReader("OggS")), nil
		}
	}
	id := register(t, h.m, Command{PaneID: "p9", Room: existingRoom, Server: h.base}).ID
	for _, name := range []string{"scribe", "planner"} {
		if r := addAgent(h.m, id, name, Command{}, h.creds); r.Error != "" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	h.m.mu.Lock()
	bots := slices.Clone(h.m.meetings[id].Bots)
	h.m.mu.Unlock()
	for i, name := range []string{"scribe", "planner"} {
		s := bots[i]
		s.mu.Lock()
		stream, whole := s.streamLocked(name), s.synthLocked(name)
		s.mu.Unlock()
		if stream == nil {
			t.Fatalf("%s: no streamed voice", name)
		}
		body, _ := stream(context.Background(), "hi")
		body.Close()
		_, _ = whole(context.Background(), "hi")
	}
	a, b := defaultVoices[DefaultTTS][0], defaultVoices[DefaultTTS][1]
	want := strings.Join(defaultVoices[DefaultTTS][:2], ",")
	if got := strings.Join(voices, ","); got != a+",whole:"+a+","+b+",whole:"+b {
		t.Fatalf("voices %v, want each of %s streamed and whole", got, want)
	}
}
