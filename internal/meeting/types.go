// SPDX-License-Identifier: Apache-2.0

// Package meeting implements a daemon-owned, explicitly started Recall.ai meeting bridge.
package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Command struct {
	Action string `json:"action"`
	// ID names the meeting (m-xxxxxx, design 043); "" = the only meeting with
	// a bot, else the only meeting. join/host without it create a meeting.
	ID        string `json:"id,omitempty"`
	URL       string `json:"url,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	Listen    string `json:"listen,omitempty"`
	Region    string `json:"region,omitempty"`
	ReadDir   string `json:"read_dir,omitempty"`
	TTS       string `json:"tts,omitempty"`  // openai | deepgram | elevenlabs (design 048)
	STT       string `json:"stt,omitempty"`  // deepgram | elevenlabs; rysh rooms only (Recall transcribes itself)
	Lang      string `json:"lang,omitempty"` // ISO 639 code the agents answer and speak in; "" = English (design 048)
	Consent   bool   `json:"consent,omitempty"`
	// KeepTranscript keeps a reporter's raw transcript on disk for this
	// meeting (H-61, 045 D5; off by default). It needs TranscriptConsent:
	// the reporter's own Consent does not cover it.
	KeepTranscript    bool `json:"keep_transcript,omitempty"`
	TranscriptConsent bool `json:"transcript_consent,omitempty"`
	AutoSpeak         bool `json:"auto_speak,omitempty"`
	// Approve restores per-turn local approval (approve, then speak). Without
	// it the bot is hands-free: an allowed speaker's request runs and the
	// reply is spoken with no terminal input (founder decision 2026-09-27).
	Approve bool `json:"approve,omitempty"`
	// Allow lists who may wake the bot: display names and/or the keywords
	// "host" and "anyone". Empty means "host". Names are labels the
	// participant chose, not authentication.
	Allow []string `json:"allow,omitempty"`
	// AllowFromFlag marks an Allow the operator typed on `##meeting add
	// agent`; an attach refuses it (the allowlist is fixed at bot start).
	// Set by the daemon, never taken from a client.
	AllowFromFlag bool   `json:"-"`
	Minutes       int    `json:"minutes,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	// Agents names ##agent-spawned agents to attach at join ("hey <name>");
	// the first one also answers "hey rysh". Agent is the attach/detach target.
	Agents []string `json:"agents,omitempty"`
	Agent  string   `json:"agent,omitempty"`
	// Model labels the session model plain "hey rysh" turns run on. Set by the
	// daemon at join, never taken from a client.
	Model string `json:"-"`
	// AuditPath is the JSONL file meeting tool calls are appended to
	// (audit.go). Set by the daemon under the session dir, never by a client.
	AuditPath string `json:"-"`
	// ZoomOBF gives Recall a zoom.obf_token_url on this listener so a Zoom
	// meeting hosted by another account can be joined on behalf of an
	// authorised Zoom user (zoomobf.go). Off by default.
	ZoomOBF bool `json:"zoom_obf,omitempty"`
	// Room names the LiveKit room `host` creates; "" = a random rysh-… name.
	Room string `json:"room,omitempty"`
	// Server, for host, is a rysh-server base URL: the room, the bot token
	// and the invite come from it instead of local LiveKit signing.
	Server string `json:"server,omitempty"`
	// PaneID is the meeting pane a `register` binds (##meeting join). Set by
	// the daemon, never taken from a client.
	PaneID string `json:"-"`
	// Reporter commands (design 045), all set by the daemon: ReportDir is
	// the meeting's report directory (<rysh dir>/meetings/<id>), Origin the
	// pane told when the report is written, Session the session's name.
	ReportDir string `json:"-"`
	Origin    string `json:"-"`
	Session   string `json:"-"`
	// report makes the bot a listen-only reporter feeding this taker.
	report *reportTaker
}
type Reply struct {
	ID             string          `json:"id,omitempty"`       // meeting id (m-xxxxxx)
	Kind           string          `json:"kind,omitempty"`     // "web" | "livekit"
	PaneID         string          `json:"pane_id,omitempty"`  // meeting pane; "" = pane-less
	Target         string          `json:"target,omitempty"`   // meeting URL (web) or room id (livekit)
	Meetings       []Reply         `json:"meetings,omitempty"` // list
	Bots           []Reply         `json:"bots,omitempty"`     // one status per bot of the meeting (D9), in start order
	State          string          `json:"state"`
	Platform       string          `json:"platform,omitempty"`
	BotID          string          `json:"bot_id,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	Speaker        string          `json:"speaker,omitempty"`
	Request        string          `json:"request,omitempty"` // explicit local `pending` only
	Result         string          `json:"result,omitempty"`  // explicit local `pending` only
	Error          string          `json:"error,omitempty"`
	Notice         string          `json:"notice,omitempty"`
	Phase          string          `json:"phase,omitempty"` // status-wall vocabulary, see Phase
	Mode           string          `json:"mode,omitempty"`  // "hands_free" or "approve"
	MediaConnected bool            `json:"media_connected"`
	SegmentsHeard  int             `json:"segments_heard,omitempty"`
	IgnoredWakes   int             `json:"ignored_wakes,omitempty"` // wakes from speakers outside the allowlist
	Allow          []string        `json:"allow,omitempty"`         // who may wake the bot in this meeting: display names, "host", "anyone"
	Secrets        map[string]bool `json:"secrets,omitempty"`
	Agent          string          `json:"agent,omitempty"`       // agent addressed by the pending turn; "" = rysh
	Agents         []string        `json:"agents,omitempty"`      // agents attached to this meeting
	Voices         []string        `json:"voices,omitempty"`      // each attached agent's TTS voice, in Agents order ("" = provider default)
	Model          string          `json:"model,omitempty"`       // model answering the pending turn
	ToolCalls      int             `json:"tool_calls"`            // audited tool calls from meeting turns
	AuditLog       string          `json:"audit_log,omitempty"`   // file holding those entries
	Room           string          `json:"room,omitempty"`        // LiveKit room (`host`)
	JoinURL        string          `json:"join_url,omitempty"`    // link for people to join that room (`host` reply only)
	Report         *ReportStatus   `json:"report,omitempty"`      // the meeting's reporter (design 045)
	LastReport     string          `json:"last_report,omitempty"` // the newest report written for this meeting
}
type Credentials struct {
	Recall, Verification, Speech string
	Zoom                         ZoomCredentials    // only with --zoom-obf
	LiveKit                      LiveKitCredentials // only for host
	RyshAPIKey                   string             // host --server: rysh API key with write
	// UpstreamAPIKey reports (doctor only) that the session has an
	// upstream.api_key, which host --server uses when RYSH_API_KEY is unset.
	UpstreamAPIKey bool
}
type Execute func(context.Context, string) (string, error)
type Synthesize func(context.Context, string) ([]byte, error)

// SynthesizeStream starts synthesis and returns the audio as it arrives
// (the provider's HTTP body); closing it abandons the request.
type SynthesizeStream func(context.Context, string) (io.ReadCloser, error)

// Agent is a Rysh agent (`##agent spawn`) attached to a meeting. Its name is
// its wake word ("hey <name>"), Voice selects its TTS voice ("" = a distinct
// default) and Execute answers as its persona. The daemon builds Execute from
// the agent's definition inside the meeting's isolated executor, never from
// the live agent actor: see design 040 §MTG-12.
type Agent struct {
	Name    string
	Voice   string
	Execute Execute
	// Model labels the model this agent's turns run on (its `model:` when
	// configured in the session, else the session model); status shows it.
	Model string
	// Note explains a fallback (e.g. an unconfigured `model:`); shown on attach.
	Note string
}

// maxAgents bounds the agents one meeting bot answers as.
const maxAgents = 4

// Platforms Recall.ai joins for us. The platform is chosen from the meeting
// URL; nothing else in the join differs today (see createPayload).
const (
	PlatformMeet  = "google_meet"
	PlatformZoom  = "zoom"
	PlatformTeams = "microsoft_teams"
	// PlatformLiveKit is a rysh-hosted LiveKit room (`rysh meeting host`).
	PlatformLiveKit = "livekit"
)

var (
	meetURL = regexp.MustCompile(`^/[a-z]{3}-[a-z]{4}-[a-z]{3}$`)
	// zoom.us / zoom.com, optionally one vanity subdomain (acme.zoom.us).
	zoomHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)?zoom\.(?:us|com)$`)
	// /j/<id>, /wc/join/<id>, /wc/<id>/join and personal /my/<name> links.
	// /s/ (host start) and zak/tk tokens carry host or registrant credentials
	// and are refused rather than forwarded to a third party.
	zoomPath = regexp.MustCompile(`^/(?:j/[0-9]{9,12}|wc/join/[0-9]{9,12}|wc/[0-9]{9,12}/join|my/[A-Za-z0-9._-]{1,64})$`)
	// Decoded thread segment: 19:meeting_…@thread.v2 (or a channel thread).
	teamsJoinPath = regexp.MustCompile(`^/l/meetup-join/19:[A-Za-z0-9_.=+-]{1,256}@thread\.[a-z0-9]{1,16}/[0-9]{1,20}$`)
	teamsMeetPath = regexp.MustCompile(`^/meet/[0-9]{6,20}$`)
)

const meetingURLHelp = "use a meeting URL: https://meet.google.com/abc-defg-hij, https://zoom.us/j/<id>?pwd=<passcode>, " +
	"https://teams.microsoft.com/l/meetup-join/… or https://teams.microsoft.com/meet/<id>?p=<passcode> (teams.live.com also accepted)"

// MeetingPlatform validates a meeting link against a strict per-platform
// allowlist (https only, exact hosts, known paths and query keys, no
// credentials or fragments) and returns the Recall platform it belongs to.
// Redirect, shortened or mail-wrapped links are refused: Recall itself only
// joins real platform URLs, and we never ask a third party to fetch an
// arbitrary host on the operator's behalf.
func MeetingPlatform(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Port() != "" {
		return "", errors.New(meetingURLHelp)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New(meetingURLHelp)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "meet.google.com":
		if u.RawQuery == "" && meetURL.MatchString(u.Path) {
			return PlatformMeet, nil
		}
		return "", errors.New("use a Google Meet URL: https://meet.google.com/abc-defg-hij (without query parameters)")
	case zoomHost.MatchString(host):
		if zoomPath.MatchString(u.Path) && onlyKeys(q, "pwd", "omn") {
			return PlatformZoom, nil
		}
		return "", errors.New("use a Zoom join URL: https://zoom.us/j/<meeting id>?pwd=<passcode> (the pwd is required for passcode meetings; start links and zak/tk tokens are refused)")
	case host == "teams.microsoft.com", host == "teams.live.com":
		if host == "teams.microsoft.com" && teamsJoinPath.MatchString(u.Path) && onlyKeys(q, "context", "anon", "deeplinkId") && teamsContext(q.Get("context")) {
			return PlatformTeams, nil
		}
		if teamsMeetPath.MatchString(u.Path) && onlyKeys(q, "p", "anon") {
			return PlatformTeams, nil
		}
		return "", errors.New("use a Teams join URL: https://teams.microsoft.com/l/meetup-join/…?context=…, https://teams.microsoft.com/meet/<id>?p=<passcode> or https://teams.live.com/meet/<id>?p=<passcode> (Safe Links and launcher wrappers are refused)")
	}
	return "", errors.New(meetingURLHelp)
}

// onlyKeys reports whether every query key is allowed and single-valued.
func onlyKeys(q url.Values, allowed ...string) bool {
	for k, v := range q {
		if len(v) != 1 || !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// teamsContext requires the meetup-join context to be the JSON object Teams
// writes ({"Tid":"…","Oid":"…"}): string values only, bounded size.
func teamsContext(s string) bool {
	if s == "" || len(s) > 1024 {
		return false
	}
	var ctx map[string]any
	if json.Unmarshal([]byte(s), &ctx) != nil || len(ctx) == 0 {
		return false
	}
	for _, v := range ctx {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return true
}

func (c *Command) Defaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8791"
	}
	if c.Region == "" {
		c.Region = "us-west-2"
	}
	if c.TTS == "" {
		c.TTS = DefaultTTS
	}
	if c.STT == "" {
		c.STT = DefaultSTT
	}
	if c.Minutes == 0 {
		c.Minutes = 60
	}
	if len(c.Allow) == 0 {
		c.Allow = []string{"host"}
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
}
func (c Command) Validate() error {
	platform, err := MeetingPlatform(c.URL)
	if err != nil {
		return err
	}
	if c.ZoomOBF {
		if platform != PlatformZoom {
			return errors.New("--zoom-obf applies only to Zoom meetings")
		}
		if _, ok := zoomMeetingNumber(c.URL); !ok {
			return errors.New("--zoom-obf needs a Zoom /j/<id> or /wc/join/<id> link (personal /my/ links carry no meeting id)")
		}
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") {
		return errors.New("--public-url must be the HTTPS URL forwarding to the meeting listener")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("--listen must use a loopback IP and port")
	}
	switch c.Region {
	case "us-west-2", "us-east-1", "eu-central-1", "ap-northeast-1":
	default:
		return errors.New("unsupported Recall region")
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
	if err := speechLangError(c.TTS, c.STT, c.Lang, false); err != nil {
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
			return errors.New("--allow entries must be non-empty display names, host or anyone")
		}
		if strings.EqualFold(strings.TrimSpace(a), "anyone") && len(c.Allow) > 1 {
			return errors.New("--allow anyone cannot be combined with other entries")
		}
	}
	if c.AutoSpeak && !c.Approve {
		return errors.New("--auto-speak only applies with --approve; hands-free mode always speaks")
	}
	if !c.Consent {
		return errors.New("join requires --consent: confirm participants know Recall processes meeting speech and Rysh uses AI-generated voice")
	}
	return nil
}

const turnTimeout = 2 * time.Minute

// decisionWindow bounds how long, in --approve mode, a captured request may
// wait for local approve, and a completed reply for local speak. Three minutes proved too
// short for a human reading the request and the reply in a live meeting.
const decisionWindow = 10 * time.Minute
