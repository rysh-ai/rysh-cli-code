// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

//go:embed media.html
var mediaPage []byte

// Manager is the session's meeting registry (design 043): many meetings
// keyed by id, each holding its bots — one per agent added with
// `##meeting add agent` (D9), or the one bot of a CLI join/host.
type Manager struct {
	mu       sync.Mutex
	meetings map[string]*Meeting
	order    []string // ids in creation order
	// starting holds the fresh (not yet listed) meetings whose bot is
	// starting: their ids are taken.
	starting map[string]*Meeting
	closed   bool
	// reportHook is told when a reporter writes a report (SetReportHook).
	reportHook func(ReportEvent)
}

// botSlot receives the Service a start creates; each start has its own, so
// starts run concurrently without touching the registry until they commit.
type botSlot struct{ bot *Service }

func NewManager() *Manager {
	return &Manager{meetings: map[string]*Meeting{}, starting: map[string]*Meeting{}}
}

// Do runs one meeting command. agents carries the resolved `##agent`
// definitions for join (--agent) and attach; other actions ignore it.
// Commands without c.ID act on the only meeting with a bot, else the only
// meeting; join and host without c.ID create a new pane-less meeting.
//
// m.mu guards only the registry and is never held across provider I/O
// (E30 PM-3c): a bot start or leave reserves its meeting (Meeting.busy)
// under the lock, talks to Recall/LiveKit/rysh-server without it, and
// commits under it again. Registry reads therefore never wait on a
// provider, and the workspace mailbox that calls them never stalls.
func (m *Manager) Do(c Command, keys Credentials, execute Execute, agents ...Agent) Reply {
	c.Defaults()
	switch c.Action {
	case "join", "host":
		return m.startBot(c, keys, execute, agents)
	case "add_agent":
		return m.addAgent(c, keys, execute, agents)
	case "remove_agent":
		return m.removeAgent(c)
	case "report_on":
		return m.reportOn(c, keys, execute)
	case "report_off", "report_now", "report_write":
		return m.reportCommand(c, execute)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Reply{Error: "meeting manager shut down"}
	}
	switch c.Action {
	case "doctor":
		m.mu.Unlock()
		return doctor(c, keys)
	case "list":
		snaps := m.snapshot()
		m.mu.Unlock()
		return list(snaps)
	case "register":
		defer m.mu.Unlock()
		return m.register(c)
	}
	mt, err := m.resolve(c.ID)
	if err != nil {
		m.mu.Unlock()
		return Reply{Error: err.Error()}
	}
	if mt == nil {
		m.mu.Unlock()
		return Reply{State: "stopped", Error: inactiveError(c.Action)}
	}
	switch c.Action {
	case "unregister":
		return m.unregister(mt)
	case "leave":
		if mt.hasAnyBot() || mt.busy != "" {
			return m.leave(mt, false)
		}
	}
	if !mt.hasBot() {
		defer m.mu.Unlock()
		switch c.Action {
		case "status", "leave":
			return mt.info()
		}
		return mt.decorate(Reply{State: "idle", Error: "meeting " + mt.ID + " has no bot"})
	}
	// Each bot has its own lock; the registry is not needed past here.
	snap := mt.copy()
	m.mu.Unlock()
	s := snap.soleBot()
	if s == nil {
		// Several agent bots (D9): status aggregates them, the stop controls
		// act on every one, and the single-bot verbs need a name to pick.
		switch c.Action {
		case "status":
			return snap.info()
		case "pause", "resume", "cancel":
			for _, b := range snap.Bots {
				b.command(c)
			}
			return snap.info()
		}
		return snap.decorate(Reply{Error: fmt.Sprintf("meeting %s has %d agent bots; %s needs a meeting with one bot (use ##meeting add/remove agent)", snap.ID, len(snap.Bots), c.Action)})
	}
	switch c.Action {
	case "attach":
		if len(agents) != 1 {
			return snap.decorate(Reply{Error: "attach needs exactly one resolved agent"})
		}
		return snap.decorate(s.attach(agents[0]))
	case "detach":
		return snap.decorate(s.detach(c.Agent))
	case "status":
		return snap.info()
	}
	return snap.decorate(s.command(c))
}

func doctor(c Command, keys Credentials) Reply {
	secrets := map[string]bool{"RECALL_API_KEY": keys.Recall != "", "RECALL_VERIFICATION_SECRET": keys.Verification != "", SpeechKeyName(c.TTS): keys.Speech != ""}
	// rysh rooms (host) listen with the STT provider; its key may be the
	// TTS one (Deepgram for both by default).
	secrets[STTKeyName(c.STT)] = secrets[STTKeyName(c.STT)] || keys.LiveKit.STTKey != ""
	if c.ZoomOBF {
		secrets[ZoomClientIDSecret] = keys.Zoom.ClientID != ""
		secrets[ZoomClientSecretSecret] = keys.Zoom.ClientSecret != ""
		secrets[ZoomRefreshTokenSecret] = keys.Zoom.RefreshToken != ""
	}
	// rysh rooms: host --server signs in to the rysh server; local host
	// signs its own LiveKit tokens (E-86).
	secrets[RyshAPIKeySecret] = keys.RyshAPIKey != ""
	secrets[UpstreamAPIKeyName] = keys.UpstreamAPIKey
	secrets[LiveKitURLSecret] = keys.LiveKit.URL != ""
	secrets[LiveKitKeySecret] = keys.LiveKit.APIKey != ""
	secrets[LiveKitSecretSecret] = keys.LiveKit.APISecret != ""
	return Reply{State: "preflight", Secrets: secrets, Notice: doctorNotice}
}

// UpstreamAPIKeyName labels the session's upstream.api_key in doctor output.
const UpstreamAPIKeyName = "upstream.api_key"

// DoctorSecretNames is the order doctor output lists its checks in.
var DoctorSecretNames = []string{"RECALL_API_KEY", "RECALL_VERIFICATION_SECRET", "OPENAI_API_KEY", "DEEPGRAM_API_KEY", "ELEVENLABS_API_KEY", ZoomClientIDSecret, ZoomClientSecretSecret, ZoomRefreshTokenSecret, RyshAPIKeySecret, UpstreamAPIKeyName, LiveKitURLSecret, LiveKitKeySecret, LiveKitSecretSecret}

const doctorNotice = "Read-only configuration check; no provider access tested. " +
	"join (Google Meet, Zoom, Teams via Recall) needs RECALL_API_KEY, RECALL_VERIFICATION_SECRET, a public HTTPS tunnel to a dedicated loopback listener and meeting consent; Recall transcribes meeting speech, retention is disabled. " +
	"host --server needs RYSH_API_KEY with the write permission (else upstream.api_key); host without --server needs LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET. " +
	"Both need the --tts key, the --stt key (rysh rooms) and a configured executor."

// join starts a Recall bot into slot. Called without m.mu (provider I/O).
func (m *Manager) join(slot *botSlot, c Command, keys Credentials, execute Execute, agents []Agent) Reply {
	if err := c.Validate(); err != nil {
		return Reply{Error: err.Error()}
	}
	if keys.Recall == "" || keys.Speech == "" && c.report == nil {
		return Reply{Error: "set RECALL_API_KEY and the selected speech provider key in this session's secrets"}
	}
	key, err := verificationKey(keys.Verification)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	if execute == nil {
		return Reply{Error: "session executor unavailable"}
	}
	bound, err := bindAgents(agents, c.TTS)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	var obf *zoomOBF
	if c.ZoomOBF {
		if obf, err = newZoomOBF(c.URL, keys.Zoom); err != nil {
			return Reply{Error: err.Error()}
		}
	}
	s, err := startService(c, keys, key, execute)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	s.obf = obf
	s.setAgents(bound)
	slot.bot = s
	return m.start(slot, s)
}

// leaveBot makes s leave its call. It takes no registry lock; ok reports
// whether the bot is gone.
func leaveBot(s *Service) (Reply, bool) {
	s.pause()
	s.mu.Lock()
	s.terminal = true
	s.admitted = false
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err := s.tr().Leave(ctx)
	cancel()
	if err != nil {
		s.mu.Lock()
		s.state = "leave_failed"
		s.notice = "Leave failed; retry leave."
		msg := err.Error()
		var failed *leaveError
		if errors.As(err, &failed) {
			s.notice, msg = failed.notice, failed.msg
		}
		s.mu.Unlock()
		reply := s.status(false)
		reply.Error = msg
		return reply, false
	}
	s.close()
	return Reply{State: "stopped", Notice: "Local bridge closed. If creation was uncertain, verify bot removal in Recall dashboard."}, true
}

// leave makes every bot of mt leave. Called with m.mu held; it releases it
// for the provider calls. On success the bots are dropped, and so is a
// pane-less meeting (nothing is left of it) or, with drop, any meeting; a
// pane meeting stays, bot-less. A failed leave with drop keeps the bots
// that failed, pane-less, until a later leave succeeds.
func (m *Manager) leave(mt *Meeting, drop bool) Reply {
	if mt.busy != "" {
		r := mt.decorate(Reply{Error: busyError(mt)})
		m.mu.Unlock()
		return r
	}
	r, ok := m.leaveBots(mt, mt.allBots())
	defer m.mu.Unlock()
	if ok {
		if drop || mt.PaneID == "" {
			m.remove(mt.ID)
		}
	} else if drop {
		mt.PaneID = ""
	}
	return mt.decorate(r)
}

// leaveBots makes bots (some or all of mt's) leave, in parallel. Called with
// m.mu held and mt not busy; it releases m.mu for the provider calls and
// returns holding it.
// Bots that left are dropped from mt; ok means all of them left. The reply
// is the bot's own for one bot, else a summary.
func (m *Manager) leaveBots(mt *Meeting, bots []*Service) (Reply, bool) {
	var who []string
	for _, s := range bots {
		s.mu.Lock()
		who = append(who, s.agentNamesLocked()...)
		s.mu.Unlock()
	}
	mt.busy, mt.busyWho = "leaving", strings.Join(who, ", ")
	m.mu.Unlock()
	replies, oks := make([]Reply, len(bots)), make([]bool, len(bots))
	var wg sync.WaitGroup
	for i, s := range bots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i], oks[i] = leaveBot(s)
		}()
	}
	wg.Wait()
	m.mu.Lock()
	mt.busy, mt.busyWho = "", ""
	all := true
	for i, s := range bots {
		if oks[i] {
			mt.dropBot(s)
		} else {
			all = false
		}
	}
	if len(bots) == 1 {
		return replies[0], all
	}
	if all {
		return Reply{State: "stopped", Notice: fmt.Sprintf("All %d bots left. Local bridges closed; if a creation was uncertain, verify removal in the Recall dashboard.", len(bots))}, true
	}
	for i := range bots {
		if !oks[i] {
			return replies[i], false
		}
	}
	return Reply{State: "stopped"}, true
}

// unregister drops mt (its pane closed, or ##meeting leave): its bot leaves
// first. Called with m.mu held; releases it.
func (m *Manager) unregister(mt *Meeting) Reply {
	switch mt.busy {
	case "starting":
		// The start commits into a meeting marked gone and makes the new
		// bot leave at once (finishStart).
		mt.gone = true
		m.remove(mt.ID)
		r := mt.decorate(Reply{State: "stopped", Notice: "The bot was still starting; it leaves as soon as the start finishes."})
		m.mu.Unlock()
		return r
	case "leaving":
		mt.PaneID = "" // pane-less: dropped when the running leave succeeds
		r := mt.decorate(Reply{State: "stopped", Notice: "The bot is already leaving."})
		m.mu.Unlock()
		return r
	}
	if !mt.hasAnyBot() {
		m.remove(mt.ID)
		r := mt.decorate(Reply{State: "stopped"})
		m.mu.Unlock()
		return r
	}
	if r := m.leave(mt, true); r.Error != "" {
		return r
	}
	return mt.decorate(Reply{State: "stopped"})
}

// busyError refuses a start or leave while another one runs on mt, naming
// what runs and where to watch it (E-82), e.g. "meeting m-4c8abf is busy
// (starting scribe) — try again when it is ready (##meeting status m-4c8abf)".
func busyError(mt *Meeting) string {
	what, when := "starting", "when it is ready"
	if mt.busy == "leaving" {
		what, when = "leaving", "when it has left"
	}
	if mt.busyWho != "" {
		what += " " + mt.busyWho
	} else {
		what += " a bot"
	}
	return "meeting " + mt.ID + " is busy (" + what + ") — try again " + when + " (##meeting status " + mt.ID + ")"
}

func inactiveError(action string) string {
	if action == "status" || action == "leave" {
		return ""
	}
	return "no active meeting"
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var bots []*Service
	for _, id := range m.order {
		// A bot whose leave is running is left by that leave; a starting
		// one by finishStart, which sees m.closed.
		if mt := m.meetings[id]; mt.busy != "leaving" {
			bots = append(bots, mt.allBots()...)
		}
	}
	m.meetings, m.order = map[string]*Meeting{}, nil
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range bots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A reporter writes what it has before the daemon goes (045 D7).
			s.finishReport("the rysh daemon stopped")
			defer s.waitReport(reportShutdownWait)
			s.pause()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.tr().Leave(ctx)
			cancel()
			s.close()
		}()
	}
	wg.Wait()
}

// start joins the new bot's transport and starts its loop; slot holds it.
func (m *Manager) start(slot *botSlot, s *Service) Reply {
	id, err := s.tr().Join(s.ctx)
	var failed *joinError
	if errors.As(err, &failed) && failed.definite {
		// Nothing exists remotely: close the local bridge instead of
		// parking in create_uncertain.
		s.close()
		slot.bot = nil
		return Reply{State: "create_failed", Error: failed.msg}
	}
	s.mu.Lock()
	if err != nil {
		s.state = "create_uncertain"
		msg := err.Error()
		if failed != nil {
			s.notice, msg = failed.notice, failed.msg
		}
		s.mu.Unlock()
		reply := s.status(false)
		reply.Error = msg
		return reply
	}
	s.botID = id
	s.state = "joining"
	s.mu.Unlock()
	go s.run()
	return s.status(false)
}
func validBotID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == '-') {
			return false
		}
	}
	return true
}

type turn struct {
	id, text, result, speaker string
	agent                     string // addressed agent; "" = rysh
	participant               int
	start, last               float64
	received                  time.Time
	failed                    bool // hands-free: the reply being spoken is the failure phrase
}

type Service struct {
	mu                   sync.Mutex
	ctx                  context.Context
	cancel               context.CancelFunc
	cfg                  Command
	transport            Transport // nil = Recall (tr)
	recall               recallClient
	verifyKey            []byte
	token                string
	execute              Execute
	speech               Synthesize
	speechStream         SynthesizeStream // optional: default-voice replies streamed to a streamSpeaker
	voice                func(ctx context.Context, voice, text string) ([]byte, error)
	voiceStream          func(ctx context.Context, voice, text string) (io.ReadCloser, error) // optional: agent-voice replies streamed
	agents               []meetingAgent                                                       // attached ##agent agents, in attach order
	primary              string                                                               // agent that answers "hey rysh"; "" = rysh itself
	noRysh               bool                                                                 // one of several agent bots in a meeting: "hey rysh" is nobody's (D9)
	unmount              func()                                                               // removes the bot from the shared listener (listener.go)
	state, botID, notice string
	admitted, paused     bool
	autoPaused           bool // paused by the bridge itself (media/admission loss), not the operator
	terminal             bool
	pending              *turn
	turnCancel           context.CancelFunc
	generation           uint64
	conn                 *websocket.Conn
	seen                 map[string]time.Time
	prefixes             map[int]time.Time
	obf                  *zoomOBF // --zoom-obf only; immutable after join
	waitNotice           string   // the host-action notice set while joining; cleared once admitted
	heard                int      // finalized segments considered while listening; count only, never content
	toolCalls            int      // audited tool calls finished in meeting turns (audit.go)
	ignored              int      // wakes from speakers outside the allowlist; count only
	cooldown             time.Time
	speakingSince        time.Time
	wall                 wallTracker
	report               *reportTaker // listen-only reporter bot (design 045); never wakes or speaks
}

func startService(c Command, keys Credentials, verifyKey []byte, execute Execute) (*Service, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.New("cannot generate meeting capability")
	}
	base, err := url.Parse(c.PublicURL)
	if err != nil {
		return nil, errors.New("--public-url must be the HTTPS URL forwarding to the meeting listener")
	}
	s := newService(c, keys, execute)
	s.verifyKey, s.token = verifyKey, hex.EncodeToString(b)
	s.recall = recallClient{base: "https://" + c.Region + ".recall.ai/api/v1", key: keys.Recall, http: newHTTPClient()}
	if s.unmount, err = mountBot(c.Listen, strings.TrimRight(base.Path, "/"), s); err != nil {
		s.cancel()
		return nil, err
	}
	return s, nil
}

// newService builds the transport-independent part of a Service and starts
// its progress-wall loop; the caller adds the transport.
func newService(c Command, keys Credentials, execute Execute) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{ctx: ctx, cancel: cancel, cfg: c, execute: execute, speech: newSpeech(speechSpec(c.TTS, c.Lang), keys.Speech), voice: newVoiceSpeech(speechSpec(c.TTS, c.Lang), keys.Speech), state: "creating", seen: map[string]time.Time{}, prefixes: map[int]time.Time{}, report: c.report}
	go s.wallLoop()
	return s
}

// bindAgents resolves the agents attached at join/host.
func bindAgents(agents []Agent, tts string) ([]meetingAgent, error) {
	var bound []meetingAgent
	for _, a := range agents {
		b, err := bindAgent(a, tts, bound)
		if err != nil {
			return nil, errors.New("agent " + a.Name + ": " + err.Error())
		}
		bound = append(bound, b)
	}
	return bound, nil
}

func (s *Service) setAgents(bound []meetingAgent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents = bound
	if len(bound) > 0 {
		s.primary = bound[0].Name
	}
	for _, b := range bound {
		if b.Note != "" {
			s.notice = strings.TrimSpace(s.notice + " " + b.Name + ": " + b.Note)
		}
	}
}

func (s *Service) close() {
	s.finishReport("the reporter left the meeting")
	s.cancel()
	s.pause()
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()
	if s.unmount != nil {
		s.unmount()
	}
}
func (s *Service) status(contents bool) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(contents)
}

// platformLocked is the meeting platform this bot is in: from the meeting
// URL for Recall, PlatformLiveKit for a rysh room.
func (s *Service) platformLocked() string {
	if s.tr().Kind() == "livekit" {
		return PlatformLiveKit
	}
	platform, _ := MeetingPlatform(s.cfg.URL)
	return platform
}

func (s *Service) statusLocked(contents bool) Reply {
	mode := "hands_free"
	if s.cfg.Approve {
		mode = "approve"
	}
	platform := s.platformLocked()
	r := Reply{State: s.state, Phase: Phase(s.state, s.pending != nil && s.pending.failed), Mode: mode, Platform: platform, BotID: s.botID, Notice: s.notice, MediaConnected: s.mediaReadyLocked(), SegmentsHeard: s.heard, IgnoredWakes: s.ignored, Allow: s.allowListLocked(), Agents: s.agentNamesLocked(), Voices: s.voicesLocked(), ToolCalls: s.toolCalls, AuditLog: s.cfg.AuditPath, Room: s.cfg.Room}
	if s.report != nil {
		r.Mode, r.Allow = "reporter", nil
	}
	if s.pending != nil {
		r.TurnID = s.pending.id
		r.Agent = s.pending.agent
		r.Model = s.cfg.Model
		if a, ok := s.agentLocked(s.pending.agent); ok && a.Model != "" {
			r.Model = a.Model
		}
		if contents {
			r.Request = s.pending.text
			r.Result = s.pending.result
			r.Speaker = s.pending.speaker
		}
	}
	return r
}
func (s *Service) clearLocked() {
	s.generation++
	if s.turnCancel != nil {
		s.turnCancel()
		s.turnCancel = nil
	}
	s.pending = nil
	s.prefixes = map[int]time.Time{}
	s.tr().ClearLocked()
	s.cooldown = time.Now().Add(2 * time.Second)
}
func (s *Service) pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	s.autoPaused = false
	s.clearLocked()
	if !s.terminal {
		s.state = "paused"
	}
}

// autoPauseLocked stops the turn because the bridge lost something it needs
// (media socket, admission, provider status). Unlike an operator pause,
// hands-free mode resumes by itself once the loss recovers.
func (s *Service) autoPauseLocked(notice string) {
	s.clearLocked()
	s.paused = true
	s.autoPaused = true
	if !s.terminal {
		s.state = "paused"
		s.notice = notice
	}
}
func (s *Service) command(c Command) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch c.Action {
	case "status":
	case "pending":
		return s.statusLocked(true)
	case "pause":
		if s.terminal {
			return s.statusLocked(false)
		}
		s.paused = true
		s.autoPaused = false
		s.clearLocked()
		s.state = "paused"
		s.notice = s.pauseNotice()
	case "cancel":
		// Drop the current turn and its playback, keep listening: the same
		// effect as a spoken "rysh stop". A paused bridge stays paused.
		if s.terminal {
			return s.statusLocked(false)
		}
		s.clearLocked()
		if !s.paused {
			s.state = "listening"
		}
		s.notice = "Current turn cancelled locally"
	case "resume":
		if s.terminal || !s.admitted || !s.mediaReadyLocked() || s.ctx.Err() != nil {
			return Reply{Error: "meeting is not ready; check admission and media connection"}
		}
		s.paused = false
		s.autoPaused = false
		s.clearLocked()
		s.state = "listening"
		s.notice = ""
	case "approve", "reject", "speak":
		if !s.cfg.Approve && c.Action != "reject" {
			return Reply{Error: "hands-free mode runs and speaks turns automatically; join with --approve for local approval, or use cancel"}
		}
		if s.pending == nil || c.TurnID == "" || s.pending.id != c.TurnID {
			return Reply{Error: "turn not found or expired; inspect meeting pending"}
		}
		if c.Action == "reject" {
			s.clearLocked()
			s.state = "listening"
			return s.statusLocked(false)
		}
		if s.paused || !s.admitted || !s.mediaReadyLocked() {
			return Reply{Error: "meeting not ready"}
		}
		if c.Action == "approve" {
			if s.state != "awaiting_approval" {
				return Reply{Error: "request is not awaiting approval"}
			}
			s.beginExecutionLocked()
		} else {
			if s.state != "awaiting_speech" {
				return Reply{Error: "no completed reply awaiting speech approval"}
			}
			s.beginSpeechLocked()
		}
	default:
		return Reply{Error: "unknown meeting command"}
	}
	return s.statusLocked(false)
}
func (s *Service) beginExecutionLocked() {
	execute, ok := s.executorLocked(s.pending.agent)
	if !ok {
		s.failLocked("The addressed agent was detached; nothing ran.")
		return
	}
	s.state = "executing"
	s.generation++
	gen := s.generation
	text := s.pending.text
	ctx, cancel := context.WithTimeout(s.ctx, turnTimeout)
	s.turnCancel = cancel
	ctx = s.withActivity(ctx, s.pending.id, gen)
	ctx = s.withTurn(ctx, s.pending)
	go func() {
		result, err := execute(ctx, replyInLanguage(s.cfg.Lang, text))
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if gen != s.generation || s.pending == nil || s.paused {
			return
		}
		s.turnCancel = nil
		if err != nil || strings.TrimSpace(result) == "" {
			s.failLocked("Executor failed or did not complete; no success reply was spoken.")
			return
		}
		if len(result) > 6000 {
			s.failLocked("Executor reply exceeds the speech limit; no reply audio generated.")
			return
		}
		s.pending.result = result
		s.state = "awaiting_speech"
		if s.cfg.AutoSpeak || !s.cfg.Approve {
			s.beginSpeechLocked()
		}
	}()
}

// failLocked ends a turn that produced no speakable reply. With --approve it
// waits for the operator's reject; hands-free it says failurePhrase briefly
// (in the meeting's language, phrases.go) instead of going silent, then
// returns to listening.
func (s *Service) failLocked(notice string) {
	if s.cfg.Approve {
		s.state = "failed"
		s.notice = notice + " Reject this turn to continue."
		return
	}
	s.notice = notice
	s.pending.failed = true
	s.pending.result = spokenPhrase(phraseFailure, s.cfg.Lang)
	s.beginSpeechLocked()
}
func (s *Service) beginSpeechLocked() {
	if ss, ok := s.tr().(streamSpeaker); ok {
		if stream := s.streamLocked(s.pending.agent); stream != nil {
			s.beginStreamedSpeechLocked(ss, stream)
			return
		}
	}
	s.state = "synthesizing"
	s.generation++
	gen := s.generation
	speech := s.synthLocked(s.pending.agent)
	result := s.pending.result
	id := s.pending.id
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	s.turnCancel = cancel
	go func() {
		audio, err := speech(ctx, result)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if gen != s.generation || s.pending == nil || s.paused {
			return
		}
		s.turnCancel = nil
		if err != nil {
			if s.cfg.Approve {
				s.state = "awaiting_speech"
				s.notice = "Speech synthesis failed; retry speak or reject the turn."
				return
			}
			if !s.pending.failed {
				s.failLocked("Speech synthesis failed for the reply (" + err.Error() + ").")
				return
			}
			// The failure phrase itself could not be synthesized: nothing
			// can be said, so go back to listening with the reason kept.
			s.clearLocked()
			s.state = "listening"
			s.notice = strings.TrimSpace(s.notice + " Speech synthesis also failed for the failure notice.")
			return
		}
		if !s.mediaReadyLocked() {
			s.autoPauseLocked("Output media disconnected")
			return
		}
		if s.tr().SpeakLocked(id, audio) != nil {
			s.autoPauseLocked("Audio delivery failed; the request was dropped.")
			return
		}
		s.state = "speaking"
		s.speakingSince = time.Now()
	}()
}

// beginStreamedSpeechLocked plays the reply while it is synthesized: first
// audio goes out as soon as the provider sends its first Ogg page instead of
// after the whole file. Stop/cancel/pause clear the transport, which closes
// the stream and so abandons the provider request mid-reply.
func (s *Service) beginStreamedSpeechLocked(ss streamSpeaker, stream SynthesizeStream) {
	s.state = "synthesizing"
	s.generation++
	gen := s.generation
	result := s.pending.result
	id := s.pending.id
	// The request lives as long as playback, bounded; closing the body ends it.
	ctx, cancel := context.WithTimeout(s.ctx, turnTimeout)
	s.turnCancel = cancel
	go func() {
		body, err := stream(ctx, result)
		s.mu.Lock()
		defer s.mu.Unlock()
		if gen != s.generation || s.pending == nil || s.paused {
			if body != nil {
				_ = body.Close()
			}
			cancel()
			return
		}
		s.turnCancel = nil
		if err != nil {
			cancel()
			if !s.pending.failed {
				s.failLocked("Speech synthesis failed for the reply (" + err.Error() + ").")
				return
			}
			s.clearLocked()
			s.state = "listening"
			s.notice = strings.TrimSpace(s.notice + " Speech synthesis also failed for the failure notice.")
			return
		}
		body = cancelOnClose{body, cancel}
		if !s.mediaReadyLocked() {
			_ = body.Close()
			s.autoPauseLocked("Output media disconnected")
			return
		}
		if ss.SpeakStreamLocked(id, body) != nil {
			_ = body.Close()
			s.autoPauseLocked("Audio delivery failed; the request was dropped.")
			return
		}
		s.state = "speaking"
		s.speakingSince = time.Now()
	}()
}

// cancelOnClose ends the synthesis request's context when the stream is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (s *Service) run() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	deadline := time.NewTimer(time.Duration(s.cfg.Minutes) * time.Minute)
	defer deadline.Stop()
	joined := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-deadline.C:
			s.expire()
			s.finishReport("the reporter's time limit was reached")
			return
		case <-poll.C:
			if !s.poll() {
				return
			}
			s.mu.Lock()
			timeout := !s.admitted && time.Since(joined) > 5*time.Minute
			s.mu.Unlock()
			if timeout {
				s.expire()
				s.finishReport("the reporter was not admitted within 5 minutes")
				return
			}
		case now := <-tick.C:
			s.mu.Lock()
			s.settleLocked(now)
			if s.pending != nil && (s.state == "awaiting_approval" || s.state == "awaiting_speech" || s.state == "failed") && now.Sub(s.pending.received) > decisionWindow {
				s.clearLocked()
				s.state = "listening"
				s.notice = "Pending turn expired"
			}
			if s.state == "speaking" && now.Sub(s.speakingSince) > 2*time.Minute {
				s.autoPauseLocked("Playback acknowledgment timed out")
			}
			s.recoverLocked()
			for id, t := range s.seen {
				if now.Sub(t) > 10*time.Minute {
					delete(s.seen, id)
				}
			}
			for id, t := range s.prefixes {
				if now.Sub(t) > 5*time.Second {
					delete(s.prefixes, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// settleLocked ends a capture once the speaker's finalized transcript has
// gone quiet: hands-free it starts the executor, with --approve it waits for
// the operator.
// defaultCaptureSettle is how long a capture waits for more finalized
// segments from the same speaker when the transport's STT does not mark the
// end of an utterance itself (Recall).
const defaultCaptureSettle = 1200 * time.Millisecond

func (s *Service) captureSettleLocked() time.Duration {
	if c, ok := s.tr().(captureSettler); ok {
		return c.CaptureSettle()
	}
	return defaultCaptureSettle
}

func (s *Service) settleLocked(now time.Time) {
	if s.pending == nil || s.state != "capturing" || now.Sub(s.pending.received) < s.captureSettleLocked() {
		return
	}
	switch {
	case strings.TrimSpace(s.pending.text) == "":
		if now.Sub(s.pending.received) > 5*time.Second {
			s.clearLocked()
			s.state = "listening"
		}
	case s.cfg.Approve:
		s.state = "awaiting_approval"
	default:
		s.beginExecutionLocked()
	}
}

// recoverLocked resumes a hands-free bridge the bridge paused itself, once
// admission and the media socket are back. Operator pauses stay sticky, and
// --approve mode keeps the explicit resume.
func (s *Service) recoverLocked() {
	if s.cfg.Approve || !s.paused || !s.autoPaused || s.terminal || !s.admitted || !s.hearingLocked() || s.ctx.Err() != nil {
		return
	}
	s.paused = false
	s.autoPaused = false
	s.clearLocked()
	s.state = "listening"
	s.notice = "Resumed automatically"
}

// poll applies one transport status observation. false ends the loop.
func (s *Service) poll() bool {
	s.mu.Lock()
	if s.terminal {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	st, err := s.tr().Status(s.ctx)
	if err != nil {
		s.mu.Lock()
		if !s.paused || s.autoPaused {
			s.autoPauseLocked(err.Error())
		}
		s.admitted = false
		s.mu.Unlock()
		return true
	}
	if st.Unknown {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return false
	}
	switch {
	case st.Admitted:
		s.admitted = true
		if s.waitNotice != "" && s.notice == s.waitNotice {
			// Admitted: "in the lobby, admit it" no longer holds.
			s.notice = ""
		}
		s.waitNotice = ""
		if !s.paused && s.state == "joining" && s.hearingLocked() {
			s.state = "listening"
		}
	case st.Ended:
		s.terminal = true
		s.clearLocked()
		s.paused = true
		s.admitted = false
		s.state = "ended"
		s.notice = "Meeting ended or bot was removed; leave to close local bridge"
		if st.Why != "" {
			// Recall's reason, e.g. a Teams lobby timeout or a Zoom join refusal.
			s.notice = "Meeting ended or bot was removed (Recall: " + st.Why + "); leave to close local bridge"
		}
		s.finishReport("the meeting ended, or the reporter was removed from it")
		return false
	default:
		s.admitted = false
		if s.state == "joining" && st.Waiting != "" {
			s.notice, s.waitNotice = st.Waiting, st.Waiting
		}
		if s.state != "joining" && (!s.paused || s.autoPaused) {
			s.autoPauseLocked(st.Lost)
		}
	}
	return true
}
func (s *Service) expire() {
	s.mu.Lock()
	s.terminal = true
	s.paused = true
	s.admitted = false
	s.clearLocked()
	s.state = "ended"
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := s.tr().Leave(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = "ended"
	s.notice = "Meeting duration/admission limit reached; leave to close bridge"
	if err != nil {
		s.state = "leave_failed"
		s.notice = "Duration limit reached; Recall leave failed. Retry leave or remove bot in dashboard."
		if s.tr().Kind() == "livekit" {
			s.notice = "Duration limit reached; leaving the LiveKit room failed. Retry leave."
		}
	}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	base, _ := url.Parse(s.cfg.PublicURL)
	switch r.URL.Path {
	case base.Path + "/media":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; media-src blob:; frame-ancestors 'none'")
		_, _ = w.Write(mediaPage)
	case base.Path + "/events":
		s.webhook(w, r)
	case base.Path + "/socket":
		s.socket(w, r)
	case base.Path + "/zoom/obf":
		s.serveZoomOBF(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (s *Service) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		w.WriteHeader(413)
		return
	}
	id, err := verifyRequest(r.Header, body, s.verifyKey, time.Now())
	if err != nil {
		w.WriteHeader(401)
		return
	}
	var event recallEvent
	if json.Unmarshal(body, &event) != nil {
		w.WriteHeader(400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.botID == "" {
		w.WriteHeader(503)
		return
	}
	if event.Data.Bot.ID != s.botID {
		w.WriteHeader(403)
		return
	}
	if _, ok := s.seen[id]; ok {
		w.WriteHeader(200)
		return
	}
	if len(s.seen) >= 4096 {
		w.WriteHeader(503)
		return
	}
	s.seen[id] = time.Now()
	if event.Event == "participant_events.leave" && s.pending != nil && event.Data.Data.Participant.ID != nil && *event.Data.Data.Participant.ID == s.pending.participant {
		s.clearLocked()
		s.state = "listening"
	}
	if event.Event == "transcript.data" {
		if u, ok := event.segment(); ok {
			s.acceptLocked(u, time.Now())
		}
	}
	w.WriteHeader(200)
}

type recallEvent struct {
	Event string `json:"event"`
	Data  struct {
		Bot struct {
			ID string `json:"id"`
		} `json:"bot"`
		Data struct {
			Participant struct {
				ID     *int   `json:"id"`
				Name   string `json:"name"`
				IsHost bool   `json:"is_host"`
			} `json:"participant"`
			Words []struct {
				Text  string `json:"text"`
				Start struct {
					Relative float64 `json:"relative"`
				} `json:"start_timestamp"`
				End *struct {
					Relative float64 `json:"relative"`
				} `json:"end_timestamp"`
			} `json:"words"`
		} `json:"data"`
	} `json:"data"`
}

// segment converts a Recall transcript event; ok is false for events the
// bridge ignores (no participant, the bot's own voice, no end timestamp).
func (e recallEvent) segment() (Segment, bool) {
	d := e.Data.Data
	if d.Participant.ID == nil || len(d.Words) == 0 || isBotSpeaker(d.Participant.Name) {
		return Segment{}, false
	}
	last := d.Words[len(d.Words)-1].End
	if last == nil {
		return Segment{}, false
	}
	words := make([]string, 0, len(d.Words))
	for _, w := range d.Words {
		words = append(words, w.Text)
	}
	return Segment{Participant: *d.Participant.ID, Name: d.Participant.Name, Host: d.Participant.IsHost, Start: d.Words[0].Start.Relative, End: last.Relative, Words: words}, true
}

// acceptLocked takes one finalized segment from any transport: stop phrases,
// wake detection, allowlist, and capture of the request that follows.
func (s *Service) acceptLocked(u Segment, now time.Time) {
	if !s.admitted || s.paused || !s.hearingLocked() {
		return
	}
	if len(u.Words) == 0 || len(u.Words) > 300 {
		return
	}
	if len(u.Name) > 200 {
		u.Name = u.Name[:200]
	}
	speaker, start := u.Participant, u.Start
	if speaker < 0 {
		return
	}
	if math.IsNaN(start) || math.IsInf(start, 0) || start < 0 || math.IsNaN(u.End) || math.IsInf(u.End, 0) || u.End < start || u.End-start > 30 {
		return
	}
	words := make([]string, 0, len(u.Words))
	size := 0
	for _, w := range u.Words {
		size += len(w)
		if size > 4000 {
			return
		}
		words = append(words, w)
	}
	text := strings.TrimSpace(strings.Join(words, " "))
	digest := sha256.Sum256([]byte(strconv.Itoa(speaker) + ":" + fmt.Sprint(start) + ":" + text))
	dedup := "seg:" + hex.EncodeToString(digest[:])
	if _, ok := s.seen[dedup]; ok {
		return
	}
	s.seen[dedup] = now
	if s.report != nil {
		// A reporter only takes notes: no stop phrase, wake or turn (045).
		s.heard++
		s.report.add(u, text)
		return
	}
	fields := strings.Fields(text)
	if stopRequested(fields) || agentStopRequested(fields, s.agents) {
		// Stopping is always safe, so any participant may do it: cancel the
		// in-flight turn and its playback, keep listening. Never a new turn.
		if s.pending != nil {
			s.clearLocked()
			s.state = "listening"
			s.notice = "Stopped by a participant"
		}
		return
	}
	if now.Before(s.cooldown) || (s.state != "listening" && s.state != "capturing") {
		// One turn at a time: speech while a turn runs is dropped, not queued.
		return
	}
	if s.pending != nil {
		if s.pending.participant != speaker {
			s.clearLocked()
			s.state = "listening"
			s.notice = "Overlapping speakers; request discarded"
			return
		}
		if start < s.pending.last || u.End-s.pending.start > 30 || len(s.pending.text)+len(text) > 4000 {
			s.clearLocked()
			s.state = "listening"
			return
		}
		s.pending.text = strings.TrimSpace(s.pending.text + " " + text)
		s.pending.last = u.End
		s.pending.received = now
		return
	}
	s.heard++
	prefixed := !s.prefixes[speaker].IsZero() && now.Sub(s.prefixes[speaker]) <= 5*time.Second
	agent, offset := wakeTarget(fields, s.agents, prefixed)
	if offset < 0 {
		if len(fields) >= 1 && wakeGreetings[normWord(fields[len(fields)-1])] && len(s.prefixes) < 100 {
			s.prefixes[speaker] = now
		}
		return
	}
	if agent == "" && s.noRysh {
		// Several agent bots share this meeting: each answers only its own
		// agent's name, so no two of them answer "hey rysh" at once (D9).
		return
	}
	if offset >= len(fields) {
		// Wake phrase alone: the request follows in the next segment(s).
		offset = len(fields)
	}
	delete(s.prefixes, speaker)
	if !s.allowedLocked(u) {
		s.ignored++
		s.notice = fmt.Sprintf("Ignored wake from %q: not in --allow (default: host only)", strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, u.label())) + ignoredWakeHint(s.platformLocked(), u)
		return
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return
	}
	s.pending = &turn{id: hex.EncodeToString(b), agent: s.addresseeLocked(agent), text: strings.Join(fields[offset:], " "), speaker: strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, u.Name), participant: speaker, start: start, last: u.End, received: now}
	s.state = "capturing"
	s.notice = ""
}

// botName prefixes the bot's display name in the meeting (createPayload); its
// own voice is never a wake or stop source.
const botName = "Rysh AI"

// allowListLocked is the allowlist in effect, normalized, for status: a UI
// (company web, status wall) shows it; it grants nothing by itself.
func (s *Service) allowListLocked() []string {
	if len(s.cfg.Allow) == 0 {
		return []string{"host"}
	}
	out := make([]string, 0, len(s.cfg.Allow))
	for _, a := range s.cfg.Allow {
		a = strings.Join(strings.Fields(a), " ")
		if strings.EqualFold(a, "host") || strings.EqualFold(a, "anyone") {
			a = strings.ToLower(a)
		}
		out = append(out, a)
	}
	return out
}

// allowedLocked applies the --allow list to a waking participant. On Recall
// entries match display names, which are labels any participant can set, not
// authentication, and "host" uses Recall's is_host flag. On LiveKit entries
// match the participant identity from the signed join token, and "host" is
// the identity `rysh meeting host` minted the link for.
func (s *Service) allowedLocked(u Segment) bool {
	allow := s.cfg.Allow
	if len(allow) == 0 {
		allow = []string{"host"}
	}
	host := u.Host
	name := strings.Join(strings.Fields(u.Name), " ")
	if u.Identity != "" {
		name = u.Identity
	}
	for _, a := range allow {
		a = strings.Join(strings.Fields(a), " ")
		switch {
		case strings.EqualFold(a, "anyone"):
			return true
		case strings.EqualFold(a, "host"):
			if host {
				return true
			}
		case name != "" && strings.EqualFold(a, name):
			return true
		}
	}
	return false
}

var stopWords = map[string]bool{"stop": true, "cancel": true, "quiet": true, "enough": true, "silence": true}

// stopRequested reports a short segment addressing the bot with a stop word:
// "rysh stop", "hey rysh, stop talking", "stop, rysh", "Rish, cancel".
func stopRequested(fields []string) bool {
	if len(fields) == 0 || len(fields) > 8 {
		return false
	}
	for i := range fields {
		w := normWord(fields[i])
		if !wakeName.MatchString(w) && !wakeMerged.MatchString(w) {
			continue
		}
		for j := max(0, i-2); j <= min(len(fields)-1, i+2); j++ {
			if j != i && stopWords[normWord(fields[j])] {
				return true
			}
		}
	}
	return false
}

// Speech-to-text almost never spells "rysh"; it writes the sound. Accept the
// spellings streaming STT produces for it after a greeting, anywhere in a
// finalized segment, or alone as a segment's first word when punctuation marks
// it as an address ("Rish, ..."). A false wake from an allowed speaker runs a
// read-only turn hands-free (or waits for `approve` with --approve); the
// speaker allowlist, not the phrase, is the authority boundary. Bare "a"/"the"
// greetings stay out: a false turn occupies the bot until it finishes.
var wakeGreetings = map[string]bool{"hey": true, "hi": true, "hay": true, "hae": true, "hej": true, "okay": true, "ok": true}

// r + vowel sound + sh/s/ch sound, optional trailing vowel: rysh, rish,
// reesh, resh, rush, rich, rhys, reese, reece, rishi, ryshe, ...
var wakeName = regexp.MustCompile(`^rh?(i|y|e|ee|ea|ei|ie|u)(sh|sch|ch|ss|s|c)(h|e|i)?$`)

// Greeting and name run together: harish, heyrish, hirish, herish.
var wakeMerged = regexp.MustCompile(`^(hey|hay|hi|he|ha)rh?(i|y|e|ee|u)(sh|sch|s)h?$`)

func normWord(v string) string {
	return strings.ToLower(strings.TrimFunc(v, func(r rune) bool { return !unicode.IsLetter(r) }))
}

func (s *Service) socket(w http.ResponseWriter, r *http.Request) {
	base, _ := url.Parse(s.cfg.PublicURL)
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == base.Scheme+"://"+base.Host }, HandshakeTimeout: 5 * time.Second}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1024)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var auth struct {
		Token string `json:"token"`
	}
	if conn.ReadJSON(&auth) != nil || subtle.ConstantTimeCompare([]byte(auth.Token), []byte(s.token)) != 1 {
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	if s.terminal || s.ctx.Err() != nil || s.conn != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.conn = conn
	if s.admitted && !s.paused && s.state == "joining" {
		s.state = "listening"
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
			if !s.paused || s.autoPaused {
				notice := "Output media disconnected; resumes automatically when the page reconnects"
				if s.cfg.Approve {
					notice = "Output media disconnected; resume explicitly after reconnection"
				}
				s.autoPauseLocked(notice)
			} else {
				s.clearLocked()
			}
		}
		s.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		var event struct {
			Type string `json:"type"`
			Turn string `json:"turn"`
		}
		if conn.ReadJSON(&event) != nil {
			return
		}
		s.mu.Lock()
		switch event.Type {
		case "played":
			s.playbackLocked(event.Turn, true)
		case "playback_error":
			s.playbackLocked(event.Turn, false)
		}
		s.mu.Unlock()
	}
}

// playbackLocked records the end of one reply's playback: played returns to
// listening, a failure pauses the bridge. Stale turns are ignored.
func (s *Service) playbackLocked(turnID string, played bool) {
	if s.pending == nil || turnID != s.pending.id || s.state != "speaking" {
		return
	}
	if !played {
		notice := "Browser could not play audio"
		if s.tr().Kind() == "livekit" {
			notice = "Could not publish reply audio to the room"
		}
		s.autoPauseLocked(notice)
		return
	}
	if !s.pending.failed {
		s.notice = ""
	}
	s.pending = nil
	s.state = "listening"
	s.cooldown = time.Now().Add(2 * time.Second)
}

// pauseNotice says what keeps processing speech while paused locally.
func (s *Service) pauseNotice() string {
	if s.tr().Kind() == "livekit" {
		return "Execution and playback paused locally until resume. The bot stays in the room and speech-to-text keeps running; use leave to stop it."
	}
	return "Execution and playback paused locally until resume. Recall still processes speech; use leave to stop the bot."
}

// Phase maps a raw service state to the progress vocabulary the bot's camera
// tile shows (MTG-13): joining, listening, heard, awaiting_approval (--approve
// only), thinking, preparing_speech, speaking, error, paused, ended. failed is
// true while a hands-free turn says failurePhrase, which reads as error.
func Phase(state string, failed bool) string {
	switch state {
	case "creating", "joining":
		return "joining"
	case "listening":
		return "listening"
	case "capturing":
		return "heard"
	case "awaiting_approval", "awaiting_speech":
		return "awaiting_approval"
	case "executing":
		return "thinking"
	case "synthesizing", "speaking":
		if failed {
			return "error"
		}
		if state == "speaking" {
			return "speaking"
		}
		return "preparing_speech"
	case "failed", "leave_failed", "create_uncertain", "create_failed":
		return "error"
	case "paused":
		return "paused"
	}
	return "ended"
}
