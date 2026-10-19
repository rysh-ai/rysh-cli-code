// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// LiveKit transport (design 042, LK-2): `rysh meeting host` creates a room on
// our own LiveKit server, mints join tokens, and connects the bot as one more
// participant. No Recall, no public listener, no host admission: the bot
// dials out. Speaker identity is the participant identity in the signed join
// token, so the allowlist matches identities, not display names.

// Secret names for the LiveKit server; values live in the session store.
const (
	LiveKitURLSecret     = "LIVEKIT_URL"        // ws(s):// or http(s):// of the LiveKit server
	LiveKitKeySecret     = "LIVEKIT_API_KEY"    // API key (token issuer)
	LiveKitSecretSecret  = "LIVEKIT_API_SECRET" // API secret (token signing key)
	LiveKitJoinURLSecret = "LIVEKIT_JOIN_URL"   // optional: join page base; default a local LiveKit Meet
)

// defaultJoinPage is a locally run LiveKit Meet (`pnpm dev`), whose /custom
// page takes a server URL and a token. It is replaced by rysh-server's
// token URL once LK-3 lands.
const defaultJoinPage = "http://localhost:3000"

const (
	liveKitBotIdentity  = "rysh-bot"
	liveKitHostIdentity = "host"
)

// LiveKitCredentials are the LiveKit server and its API key pair.
type LiveKitCredentials struct {
	URL, APIKey, APISecret string
	JoinPage               string // "" = defaultJoinPage
	STTKey                 string // STTKeyName(Command.STT): per-participant streaming STT
}

func (l LiveKitCredentials) complete() bool {
	return l.URL != "" && l.APIKey != "" && l.APISecret != ""
}

// LiveKitRoom is the bot's live connection to a room. The WebRTC side —
// server-sdk-go, one streaming STT session per participant audio track, and
// publishing reply audio on the bot's own track — implements it (LK-1,
// livekit.go) and registers itself in DialLiveKit.
type LiveKitRoom interface {
	// Events delivers final utterances and participant/connection changes;
	// it is closed when the connection ends.
	Events() <-chan LiveKitEvent
	// Play publishes one reply on the bot's audio track and returns when it
	// has been sent, or when ctx is cancelled (barge-in, stop). audio is what
	// the Service's speech produced: LiveKitSpeech's format when set (LK-1
	// uses Ogg/Opus), else the MP3 of the Recall route.
	Play(ctx context.Context, audio []byte) error
	Close() error
}

// LiveKitEvent kinds.
const (
	LiveKitUtterance = "utterance" // a final STT segment from one participant
	LiveKitLeft      = "left"      // a participant left the room
)

// LiveKitEvent is one room event. Identity comes from the participant's
// signed token. Start/End are seconds since the bot joined; zero means
// "now" and the transport stamps the arrival time.
type LiveKitEvent struct {
	Kind       string
	Identity   string
	Name       string
	Text       string
	Start, End float64
}

// LiveKitDialOptions carries what the room client needs besides the token:
// the streaming STT provider (Command.STT, "" = Deepgram), the language
// (Command.Lang, "" = English) and the provider's key, for
// per-participant transcription.
type LiveKitDialOptions struct {
	STT, Lang, STTKey string
	// Keyterms is the bot's current agent names, read by the transcriber
	// to bias recognition towards them (E-85); nil = only "Rysh".
	Keyterms func() []string
}

// DialLiveKit connects the bot to a room (livekit.go registers it). If nil,
// `host` still creates the room and prints the join link, but reports that
// the bot could not join.
var DialLiveKit func(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error)

// LiveKitSpeech, when set, replaces the Service's reply synthesis for
// LiveKit rooms so the audio arrives in the format Play publishes without
// transcoding (tts is a ValidTTS provider, key its API key).
var LiveKitSpeech func(tts, key string) Synthesize

// LiveKitSpeechStream, when set, streams default-voice replies: playback
// starts with the provider's first Ogg page instead of the whole file.
var LiveKitSpeechStream func(tts, key string) SynthesizeStream

// LiveKitVoiceSpeech and LiveKitVoiceSpeechStream are the same in an
// agent's own voice. Without them every agent in a room speaks in the
// default voice — two agent bots (D9) would sound the same.
var (
	LiveKitVoiceSpeech       func(tts, key string) func(ctx context.Context, voice, text string) ([]byte, error)
	LiveKitVoiceSpeechStream func(tts, key string) func(ctx context.Context, voice, text string) (io.ReadCloser, error)
)

// liveKitStreamPlayer is a LiveKitRoom that can publish audio while it is
// still arriving; Play's read-it-all path is the fallback.
type liveKitStreamPlayer interface {
	PlayStream(ctx context.Context, audio io.ReadCloser) error
}

// liveKitCaptureSettle: a LiveKit utterance is already Deepgram-endpointed
// (speech_final / Finalize), so a capture whose request text is present runs
// at once instead of waiting Recall's 1.2 s for more segments.
const liveKitCaptureSettle = 0

var liveKitRoomName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateHost checks a `host` command. The room server comes from session
// secrets, not arguments, so only local choices are validated here.
func (c Command) ValidateHost() error {
	if c.Room != "" && !liveKitRoomName.MatchString(c.Room) {
		return errors.New("--room must be 1–64 letters, digits, '-' or '_'")
	}
	if c.Server != "" {
		if _, err := validServerURL(c.Server); err != nil {
			return err
		}
	}
	if c.URL != "" || c.ZoomOBF {
		return errors.New("host creates a rysh room; use join for Meet/Zoom/Teams URLs")
	}
	if !ValidTTS(c.TTS) {
		return errors.New("--tts (or MEETING_TTS) must be deepgram, elevenlabs or openai")
	}
	if !ValidSTT(c.STT) {
		return errors.New("--stt (or MEETING_STT) must be deepgram or elevenlabs")
	}
	if !ValidLang(c.Lang) {
		return errors.New("--lang (or MEETING_LANGUAGE) must be a 2–3 letter ISO 639 code such as en, tr or kmr")
	}
	if err := speechLangError(c.TTS, c.STT, c.Lang, true); err != nil {
		return err
	}
	if c.Minutes < 1 || c.Minutes > 180 {
		return errors.New("--minutes must be 1–180")
	}
	if len(c.Allow) > 20 {
		return errors.New("--allow accepts at most 20 entries")
	}
	for _, a := range c.Allow {
		if strings.TrimSpace(a) == "" || len(a) > 200 {
			return errors.New("--allow entries must be non-empty participant identities, host or anyone")
		}
		if strings.EqualFold(strings.TrimSpace(a), "anyone") && len(c.Allow) > 1 {
			return errors.New("--allow anyone cannot be combined with other entries")
		}
	}
	if c.AutoSpeak && !c.Approve {
		return errors.New("--auto-speak only applies with --approve; hands-free mode always speaks")
	}
	return nil
}

// liveKitEndpoints derives the API base (http/https, for room service calls)
// and the client URL (ws/wss, for join links) from LIVEKIT_URL.
func liveKitEndpoints(raw string) (api, client string, err error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("LIVEKIT_URL must be ws://, wss://, http:// or https:// of the LiveKit server")
	}
	a, c := *u, *u
	switch u.Scheme {
	case "ws", "http":
		a.Scheme, c.Scheme = "http", "ws"
	case "wss", "https":
		a.Scheme, c.Scheme = "https", "wss"
	default:
		return "", "", errors.New("LIVEKIT_URL must be ws://, wss://, http:// or https:// of the LiveKit server")
	}
	return a.String(), c.String(), nil
}

// liveKitGrant is LiveKit's "video" claim.
type liveKitGrant struct {
	Room           string `json:"room,omitempty"`
	RoomCreate     bool   `json:"roomCreate,omitempty"`
	RoomJoin       bool   `json:"roomJoin,omitempty"`
	CanPublish     *bool  `json:"canPublish,omitempty"`
	CanSubscribe   *bool  `json:"canSubscribe,omitempty"`
	CanPublishData *bool  `json:"canPublishData,omitempty"`
}

// liveKitToken mints a LiveKit access token: an HS256 JWT signed with the
// API secret, issuer = API key, subject = participant identity.
func liveKitToken(key, secret, identity, name string, grant liveKitGrant, ttl time.Duration, now time.Time) (string, error) {
	claims := map[string]any{"iss": key, "nbf": now.Add(-10 * time.Second).Unix(), "exp": now.Add(ttl).Unix(), "video": grant}
	if identity != "" {
		claims["sub"] = identity
		claims["jti"] = identity
	}
	if name != "" {
		claims["name"] = name
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// createLiveKitRoom calls RoomService.CreateRoom (Twirp JSON). Idempotent on
// LiveKit's side: an existing room of that name is returned as is.
func createLiveKitRoom(ctx context.Context, client *http.Client, api string, creds LiveKitCredentials, room string, minutes int) error {
	token, err := liveKitToken(creds.APIKey, creds.APISecret, "", "", liveKitGrant{RoomCreate: true}, time.Minute, time.Now())
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"name": room, "empty_timeout": 600, "max_participants": 20, "departure_timeout": 60, "metadata": fmt.Sprintf(`{"rysh":true,"minutes":%d}`, minutes)})
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/twirp/livekit.RoomService/CreateRoom", bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid LIVEKIT_URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return errors.New("LiveKit server unreachable; check LIVEKIT_URL and that the server runs")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		// Never echo the body: a misconfigured proxy may reflect the token.
		return fmt.Errorf("LiveKit CreateRoom returned HTTP %d; check LIVEKIT_API_KEY/LIVEKIT_API_SECRET", res.StatusCode)
	}
	return nil
}

// host implements `rysh meeting host`: create a LiveKit room, mint the join
// link, connect the bot into slot. Called without m.mu (provider I/O).
func (m *Manager) host(slot *botSlot, c Command, keys Credentials, execute Execute, agents []Agent) Reply {
	if err := c.ValidateHost(); err != nil {
		return Reply{Error: err.Error()}
	}
	if c.Server != "" {
		return m.hostOnServer(slot, c, keys, execute, agents)
	}
	lk := keys.LiveKit
	if !lk.complete() {
		return Reply{Error: fmt.Sprintf("set %s, %s and %s in this session's secrets", LiveKitURLSecret, LiveKitKeySecret, LiveKitSecretSecret)}
	}
	if err := roomSpeechKeysError(c, keys); err != nil {
		return Reply{Error: err.Error()}
	}
	if execute == nil {
		return Reply{Error: "session executor unavailable"}
	}
	api, client, err := liveKitEndpoints(lk.URL)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	page := strings.TrimRight(lk.JoinPage, "/")
	if page == "" {
		page = defaultJoinPage
	}
	if u, err := url.Parse(page); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return Reply{Error: LiveKitJoinURLSecret + " must be an http(s) URL of the join page"}
	}
	bound, err := bindAgents(agents, c.TTS)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	room := c.Room
	if room == "" {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return Reply{Error: "cannot generate a room name"}
		}
		room = "rysh-" + hex.EncodeToString(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := createLiveKitRoom(ctx, newHTTPClient(), api, lk, room, c.Minutes); err != nil {
		return Reply{State: "create_failed", Error: err.Error()}
	}
	ttl := time.Duration(c.Minutes)*time.Minute + 10*time.Minute
	yes := true
	join := liveKitGrant{Room: room, RoomJoin: true, CanPublish: &yes, CanSubscribe: &yes, CanPublishData: &yes}
	hostToken, err := liveKitToken(lk.APIKey, lk.APISecret, liveKitHostIdentity, "Host", join, ttl, time.Now())
	if err != nil {
		return Reply{Error: "cannot mint join token"}
	}
	botToken, err := liveKitToken(lk.APIKey, lk.APISecret, liveKitBotIdentity, botName, join, ttl, time.Now())
	if err != nil {
		return Reply{Error: "cannot mint bot token"}
	}
	link := page + "/custom?" + url.Values{"liveKitUrl": {client}, "token": {hostToken}}.Encode()

	c.Room = room
	t := &livekitTransport{server: client, token: botToken, room: room, botIdentity: liveKitBotIdentity, hostIdentity: liveKitHostIdentity, sttKey: lk.STTKey}
	return m.startLiveKit(slot, c, keys, execute, bound, t, link, "The room exists and the join link works for people; it closes 10 minutes after the last one leaves.")
}

// startLiveKit runs the bot in a room whose tokens are already in hand
// (local signing or rysh-server). Called without m.mu (provider I/O).
func (m *Manager) startLiveKit(slot *botSlot, c Command, keys Credentials, execute Execute, bound []meetingAgent, t *livekitTransport, link, failedNotice string) Reply {
	s := newService(c, keys, execute)
	if LiveKitSpeech != nil {
		// The Recall MP3 voice synthesis is the wrong format for Play: use
		// the Opus one, or the default voice when none is registered.
		s.speech, s.voice = LiveKitSpeech(speechSpec(c.TTS, c.Lang), keys.Speech), nil
		if LiveKitVoiceSpeech != nil {
			s.voice = LiveKitVoiceSpeech(speechSpec(c.TTS, c.Lang), keys.Speech)
		}
	}
	if LiveKitSpeechStream != nil {
		s.speechStream = LiveKitSpeechStream(speechSpec(c.TTS, c.Lang), keys.Speech)
		if LiveKitVoiceSpeechStream != nil {
			s.voiceStream = LiveKitVoiceSpeechStream(speechSpec(c.TTS, c.Lang), keys.Speech)
		}
	}
	t.s, t.dial, t.ids, t.stt, t.lang = s, DialLiveKit, map[string]int{}, c.STT, c.Lang
	s.transport = t
	s.setAgents(bound)
	slot.bot = s
	r := m.start(slot, s)
	r.Room, r.JoinURL = t.room, link
	if r.State == "create_failed" {
		r.Notice = failedNotice
	}
	return r
}

// livekitTransport connects the bot to one LiveKit room. Fields after dial
// are guarded by Service.mu.
type livekitTransport struct {
	s                  *Service
	server, token      string
	room               string
	botIdentity        string // the bot's own identity: never a wake source
	hostIdentity       string // matches the "host" allowlist keyword
	dial               func(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error)
	sttKey             string
	stt, lang          string // STT provider and language (Command.STT, Command.Lang)
	conn               LiveKitRoom
	connected          bool
	joined             time.Time
	ids                map[string]int // identity → Segment.Participant
	stopPlay           context.CancelFunc
	stream             io.Closer // the synthesis stream being played; ClearLocked closes it
	playing            string    // turn being played
	disconnectedNotice string
	wallOut            chan []byte // latest wall message for wallSender; nil before Join
}

// liveKitWallTopic is the data topic of the bot's progress wall (design 042
// LK-4): the app's MeetingRoom renders one wall per bot from it.
const liveKitWallTopic = "rysh.wall"

// liveKitDataRoom is a LiveKitRoom that can send data messages.
type liveKitDataRoom interface {
	PublishData(topic string, payload []byte) error
}

// PublishWallLocked queues the wall message for the room; a newer message
// replaces one not yet sent, and sending never blocks the Service.
func (t *livekitTransport) PublishWallLocked(payload []byte) bool {
	if !t.connected || t.wallOut == nil {
		return false
	}
	select {
	case <-t.wallOut:
	default:
	}
	t.wallOut <- payload // only producer, just drained: never blocks
	return true
}

// wallSender publishes queued wall messages until the Service ends.
func (t *livekitTransport) wallSender(room liveKitDataRoom, out <-chan []byte) {
	for {
		select {
		case <-t.s.ctx.Done():
			return
		case p := <-out:
			_ = room.PublishData(liveKitWallTopic, p)
		}
	}
}

func (*livekitTransport) Kind() string { return "livekit" }

// agentNames is the bot's attached agents now (attach/detach change them).
func (t *livekitTransport) agentNames() []string {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return t.s.agentNamesLocked()
}

func (t *livekitTransport) Join(ctx context.Context) (string, error) {
	if t.dial == nil {
		return "", &joinError{definite: true, msg: "this build has no LiveKit room client yet (design 042 LK-1), so the bot cannot join; people can use the join link"}
	}
	conn, err := t.dial(ctx, t.server, t.token, LiveKitDialOptions{STT: t.stt, Lang: t.lang, STTKey: t.sttKey, Keyterms: t.agentNames})
	if err != nil {
		// Nothing of ours is left in the room: the connection never completed.
		return "", &joinError{definite: true, msg: "bot could not join the LiveKit room: " + err.Error()}
	}
	t.s.mu.Lock()
	t.conn, t.connected, t.joined = conn, true, time.Now()
	if dr, ok := conn.(liveKitDataRoom); ok {
		out := make(chan []byte, 1)
		t.wallOut = out
		go t.wallSender(dr, out)
	}
	t.s.mu.Unlock()
	go t.pump(conn)
	return t.room, nil
}

// pump feeds room events into the Service until the connection ends.
func (t *livekitTransport) pump(conn LiveKitRoom) {
	for ev := range conn.Events() {
		s := t.s
		s.mu.Lock()
		switch ev.Kind {
		case LiveKitUtterance:
			if ev.Identity != "" && ev.Identity != t.botIdentity {
				s.acceptLocked(t.segmentLocked(ev), time.Now())
				s.settleLocked(time.Now()) // don't wait for the 250 ms tick
			}
		case LiveKitLeft:
			if id, ok := t.ids[ev.Identity]; ok && s.pending != nil && s.pending.participant == id {
				s.clearLocked()
				s.state = "listening"
			}
		}
		s.mu.Unlock()
	}
	t.s.mu.Lock()
	if t.conn == conn {
		t.connected = false
		t.disconnectedNotice = "Disconnected from the LiveKit room"
	}
	t.s.mu.Unlock()
}

func (t *livekitTransport) segmentLocked(ev LiveKitEvent) Segment {
	id, ok := t.ids[ev.Identity]
	if !ok {
		id = len(t.ids) + 1
		if len(t.ids) < 1000 {
			t.ids[ev.Identity] = id
		}
	}
	start, end := ev.Start, ev.End
	if start == 0 && end == 0 {
		start = time.Since(t.joined).Seconds()
		end = start
	}
	name := ev.Name
	if len(name) > 200 {
		name = name[:200]
	}
	return Segment{Participant: id, Name: name, Identity: ev.Identity, Host: ev.Identity == t.hostIdentity, Start: start, End: end, Words: strings.Fields(ev.Text)}
}

func (t *livekitTransport) Leave(context.Context) error {
	t.s.mu.Lock()
	conn := t.conn
	t.conn, t.connected = nil, false
	if t.stopPlay != nil {
		t.stopPlay()
		t.stopPlay = nil
	}
	t.closeStreamLocked()
	t.s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

func (t *livekitTransport) Status(context.Context) (TransportStatus, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if t.connected {
		return TransportStatus{Admitted: true}, nil
	}
	if t.conn == nil && t.disconnectedNotice == "" {
		return TransportStatus{Unknown: true}, nil
	}
	return TransportStatus{Ended: true}, nil
}

func (t *livekitTransport) MediaReadyLocked() bool { return t.connected }

func (*livekitTransport) CaptureSettle() time.Duration { return liveKitCaptureSettle }

func (t *livekitTransport) SpeakLocked(turnID string, audio []byte) error {
	return t.playLocked(turnID, func(ctx context.Context, conn LiveKitRoom) error { return conn.Play(ctx, audio) })
}

// SpeakStreamLocked plays audio while it arrives; clearing (stop, cancel,
// pause) cancels ctx, and the stream is always closed at the end.
func (t *livekitTransport) SpeakStreamLocked(turnID string, audio io.ReadCloser) error {
	t.closeStreamLocked()
	err := t.playLocked(turnID, func(ctx context.Context, conn LiveKitRoom) error {
		if sp, ok := conn.(liveKitStreamPlayer); ok {
			return sp.PlayStream(ctx, audio)
		}
		defer audio.Close()
		stop := context.AfterFunc(ctx, func() { _ = audio.Close() })
		b, err := io.ReadAll(io.LimitReader(audio, 8<<20))
		stop()
		if err != nil {
			return err
		}
		return conn.Play(ctx, b)
	})
	if err == nil {
		t.stream = audio
	}
	return err
}

// closeStreamLocked abandons the synthesis stream now, not when a context
// callback gets scheduled: once a stop, cancel or pause has returned, the
// old reply's provider request is gone, so "listening" really means it.
func (t *livekitTransport) closeStreamLocked() {
	if t.stream != nil {
		_ = t.stream.Close()
		t.stream = nil
	}
}

func (t *livekitTransport) playLocked(turnID string, play func(context.Context, LiveKitRoom) error) error {
	if !t.connected || t.conn == nil {
		return errors.New("not connected to the room")
	}
	if t.stopPlay != nil {
		t.stopPlay()
	}
	ctx, cancel := context.WithCancel(t.s.ctx)
	t.stopPlay, t.playing = cancel, turnID
	conn := t.conn
	go func() {
		err := play(ctx, conn)
		t.s.mu.Lock()
		defer t.s.mu.Unlock()
		if ctx.Err() != nil || t.playing != turnID {
			return // cleared or superseded; the Service already moved on
		}
		cancel()
		t.stopPlay, t.playing = nil, ""
		t.s.playbackLocked(turnID, err == nil)
	}()
	return nil
}

func (t *livekitTransport) ClearLocked() {
	if t.stopPlay != nil {
		t.stopPlay()
		t.stopPlay, t.playing = nil, ""
	}
	t.closeStreamLocked()
}
