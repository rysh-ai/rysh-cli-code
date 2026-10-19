// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/meetingexec"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##meeting add agent` / `##meeting remove agent` (design 043 §Agent-add
// flows, PM-3). The `##meeting` dispatcher routes its "add" and "remove"
// subcommands here.

const meetingAgentsUsage = "##meeting add agent <id> <agent> [--consent] [--public-url <https url>] [--allow host,Name,…|anyone] [--tts deepgram|elevenlabs|openai] [--stt deepgram|elevenlabs] [--lang <iso639, e.g. kmr>] | ##meeting remove agent <id> <agent>"

// Pane meta keys of the meeting pane contract updated after birth.
const (
	paneMetaMeetingAgents = "meeting.agents"
	paneMetaMeetingBot    = "meeting.bot"
	// paneMetaMeetingBots is each agent bot's phase, "<agent>:<phase>,…"
	// (design 043 D9); meeting.bot stays the aggregate for older apps.
	paneMetaMeetingBots = "meeting.bots"
)

// MeetingPublicURLVar is the session variable `add agent` takes the Recall
// bot's public URL from when --public-url is not given.
const MeetingPublicURLVar = "MEETING_PUBLIC_URL"

// MeetingRecallRegionVar is the session variable naming the Recall region
// the session's RECALL_API_KEY belongs to (Recall keys are regional); unset
// = the `rysh meeting join` default, us-west-2.
const MeetingRecallRegionVar = "MEETING_RECALL_REGION"

// MeetingAllowVar is the session variable `add agent` takes the bot's
// allowlist from when --allow is not given ("host,Display Name,…" or
// "anyone"); unset = "host".
const MeetingAllowVar = "MEETING_ALLOW"

// MeetingTTSVar and MeetingSTTVar are the session variables choosing the
// speech providers (design 048) when --tts / --stt are not given: TTS
// deepgram | elevenlabs | openai, STT deepgram | elevenlabs; unset = Deepgram.
const (
	MeetingTTSVar = "MEETING_TTS"
	MeetingSTTVar = "MEETING_STT"
	// MeetingLanguageVar is the ISO 639 code the agents answer and speak in
	// when --lang is not given (e.g. kmr for Kurmanji); unset = English.
	MeetingLanguageVar = "MEETING_LANGUAGE"
)

// meetingSpeechVars fills c's unset speech providers from the session
// variables; an explicit flag wins. Validation stays with the meeting
// package, so a bad variable is reported like a bad flag.
func (w *WorkspaceActor) meetingSpeechVars(c *meeting.Command, paneID string) {
	if w.variables == nil {
		return
	}
	chain := w.secretScopeChain(paneID)
	if c.TTS == "" {
		v, _, _, _ := w.variables.GetLayered(chain, MeetingTTSVar)
		c.TTS = strings.ToLower(strings.TrimSpace(v))
	}
	if c.STT == "" {
		v, _, _, _ := w.variables.GetLayered(chain, MeetingSTTVar)
		c.STT = strings.ToLower(strings.TrimSpace(v))
	}
	if c.Lang == "" {
		v, _, _, _ := w.variables.GetLayered(chain, MeetingLanguageVar)
		c.Lang = strings.ToLower(strings.TrimSpace(v))
	}
}

// splitAllow parses an --allow value like `rysh meeting join --allow`.
func splitAllow(v string) []string {
	var out []string
	for _, a := range strings.Split(v, ",") {
		out = append(out, strings.TrimSpace(a))
	}
	return out
}

// Seams for tests: the agent and session executor need a live agentic
// setup, and pane output/meta go over NATS.
var (
	meetingAgentResolver = func(w *WorkspaceActor, name string) (meeting.Agent, error) { return w.meetingAgent(name) }
	meetingSessionExec   = func(w *WorkspaceActor) (meeting.Execute, error) {
		if w.agSetup == nil {
			return nil, nil // Manager answers "session executor unavailable"
		}
		return meetingexec.New(w.actorSystem, w.agSetup.Provider, "")
	}
	meetingPaneNotice = func(w *WorkspaceActor, paneID, text string) { meetingNotice(w.pub, paneID, text) }
	meetingPaneMeta   = func(w *WorkspaceActor, paneID, key, value string) {
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetMeta{Key: key, Value: value})
	}
	meetingBotWatch = 2 * time.Second // poll interval while the bot joins
)

// meetingBotWatchFor outlasts Recall's waiting_room_timeout (300 s): a host
// may admit the bot at any point until Recall gives up.
const meetingBotWatchFor = 6 * time.Minute

type meetingAgentArgs struct {
	add              bool
	id, agent        string
	consent          bool
	publicURL        string
	publicURLFromCmd bool
	allow            []string
	allowFromCmd     bool
	tts, stt         string // --tts / --stt; "" = MEETING_TTS / MEETING_STT, else Deepgram
	lang             string // --lang; "" = MEETING_LANGUAGE, else English
}

func parseMeetingAgentArgs(args []string) (meetingAgentArgs, error) {
	var a meetingAgentArgs
	if len(args) < 4 || (args[0] != "add" && args[0] != "remove") || args[1] != "agent" {
		return a, fmt.Errorf("usage: %s", meetingAgentsUsage)
	}
	a.add, a.id, a.agent = args[0] == "add", args[2], args[3]
	for i := 4; i < len(args); i++ {
		switch {
		case a.add && args[i] == "--consent":
			a.consent = true
		case a.add && args[i] == "--public-url" && i+1 < len(args):
			a.publicURL, a.publicURLFromCmd = args[i+1], true
			i++
		case a.add && strings.HasPrefix(args[i], "--public-url="):
			a.publicURL, a.publicURLFromCmd = strings.TrimPrefix(args[i], "--public-url="), true
		case a.add && args[i] == "--lang" && i+1 < len(args):
			a.lang = strings.ToLower(args[i+1])
			i++
		case a.add && strings.HasPrefix(args[i], "--lang="):
			a.lang = strings.ToLower(strings.TrimPrefix(args[i], "--lang="))
		case a.add && (args[i] == "--tts" || args[i] == "--stt") && i+1 < len(args):
			if args[i] == "--tts" {
				a.tts = args[i+1]
			} else {
				a.stt = args[i+1]
			}
			i++
		case a.add && (strings.HasPrefix(args[i], "--tts=") || strings.HasPrefix(args[i], "--stt=")):
			if v, ok := strings.CutPrefix(args[i], "--tts="); ok {
				a.tts = v
			} else {
				a.stt = strings.TrimPrefix(args[i], "--stt=")
			}
		case a.add && (args[i] == "--allow" && i+1 < len(args) || strings.HasPrefix(args[i], "--allow=")):
			// ## input is split on spaces with no quoting, and display names
			// have spaces: the value runs to the next --flag, and one pair of
			// surrounding quotes is dropped (--allow "host,Halil Ağın").
			words := []string{strings.TrimPrefix(args[i], "--allow=")}
			if args[i] == "--allow" {
				words = nil
			}
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
				words = append(words, args[i])
			}
			v := strings.Join(words, " ")
			if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
			a.allow, a.allowFromCmd = splitAllow(v), true
		default:
			return a, fmt.Errorf("usage: %s", meetingAgentsUsage)
		}
	}
	if a.tts != "" && !meeting.ValidTTS(a.tts) {
		return a, fmt.Errorf("--tts %q: use deepgram, elevenlabs or openai", a.tts)
	}
	if a.stt != "" && !meeting.ValidSTT(a.stt) {
		return a, fmt.Errorf("--stt %q: use deepgram or elevenlabs", a.stt)
	}
	if !meeting.ValidLang(a.lang) {
		return a, fmt.Errorf("--lang %q: use a 2–3 letter ISO 639 code such as en, tr or kmr", a.lang)
	}
	if !meeting.ValidMeetingID(a.id) {
		return a, fmt.Errorf("%q is not a meeting id (m-xxxxxx); see ##meeting list", a.id)
	}
	return a, nil
}

// meetingAgentCommand handles `##meeting add|remove agent …`; args start at
// "add"/"remove". Provider calls run off the mailbox; results land in paneID.
func (w *WorkspaceActor) meetingAgentCommand(out *strings.Builder, paneID string, args []string) {
	a, err := parseMeetingAgentArgs(args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage: ") {
			ryshWriter(out).UsageLineIn("meeting", meetingAgentsUsage)
			w.failRyshUsage("%v", err)
			return
		}
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	if a.agent == meeting.ReporterName {
		// `add agent <id> reporter` is the meeting reporter (design 045).
		if a.allowFromCmd {
			err := fmt.Errorf("the reporter answers nobody, so --allow does not apply to it")
			fmt.Fprintf(out, "\n[meeting] %v\n", err)
			w.failRysh("%v", err)
			return
		}
		verb := "off"
		if a.add {
			verb = "on"
		}
		w.meetingReport(out, paneID, meetingReportArgs{id: a.id, verb: verb, consent: a.consent, publicURL: a.publicURL, publicURLFromCmd: a.publicURLFromCmd})
		return
	}
	var mt meeting.Meeting
	ok := false
	if w.meeting != nil {
		mt, ok = w.meeting.Lookup(a.id)
	}
	if !ok {
		err := fmt.Errorf("no meeting %s; see ##meeting list", a.id)
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	// A meeting whose bot is starting or leaving refuses the add/remove
	// (design 043: busy refuses, no queue). Say so here, in this command's
	// own output, instead of after "starting a bot …" from a goroutine
	// (E-82). The registry still decides; a start that slips in between is
	// refused there and reported by meetingAgentsDone.
	if e := mt.BusyError(); e != "" {
		fmt.Fprintf(out, "\n[meeting] %s\n", e)
		w.failRysh("%s", e)
		return
	}
	manager, fx := w.meeting, currentMeetingEffects()
	if !a.add {
		fmt.Fprintf(out, "\n[meeting] %s: removing agent %q …\n", a.id, a.agent)
		go func() {
			r := manager.Do(meeting.Command{Action: "remove_agent", ID: a.id, Agent: a.agent}, meeting.Credentials{}, nil)
			w.meetingAgentsDone(fx, paneID, a, r)
		}()
		return
	}

	agent, err := meetingAgentResolver(w, a.agent)
	if err != nil {
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	if a.allowFromCmd && len(mt.Bots) > 0 {
		err := fmt.Errorf("meeting %s already has its bots; its allowlist was fixed when the first started (##meeting status %s). Add the agent without --allow, or leave and add again", a.id, a.id)
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	c := meeting.Command{Action: "add_agent", ID: a.id, Consent: a.consent, PublicURL: a.publicURL, Allow: a.allow, AllowFromFlag: a.allowFromCmd, TTS: a.tts, STT: a.stt, Lang: a.lang}
	if !a.allowFromCmd && len(mt.Bots) == 0 && w.variables != nil {
		if v, _, _, _ := w.variables.GetLayered(w.secretScopeChain(paneID), MeetingAllowVar); strings.TrimSpace(v) != "" {
			c.Allow = splitAllow(v)
		}
	}
	w.meetingSpeechVars(&c, paneID)
	c.Defaults()
	var keys meeting.Credentials
	var execute meeting.Execute
	{
		// Every agent starts its own bot (design 043 D9): resolve what
		// 040/042 need.
		chain := w.secretScopeChain(paneID)
		secret := func(name string) string {
			if w.secrets == nil {
				return ""
			}
			v, _, _, _ := w.secrets.GetLayered(chain, name)
			return v
		}
		keys.Speech = secret(meeting.SpeechKeyName(c.TTS))
		switch mt.Kind {
		case meeting.KindWeb:
			keys.Recall, keys.Verification = secret("RECALL_API_KEY"), secret("RECALL_VERIFICATION_SECRET")
			if w.variables != nil {
				if !a.publicURLFromCmd {
					c.PublicURL, _, _, _ = w.variables.GetLayered(chain, MeetingPublicURLVar)
				}
				if region, _, _, _ := w.variables.GetLayered(chain, MeetingRecallRegionVar); strings.TrimSpace(region) != "" {
					c.Region = strings.TrimSpace(region)
				}
				c.Defaults()
			}
		case meeting.KindLiveKit:
			keys.LiveKit.STTKey = secret(meeting.STTKeyName(c.STT))
			// Same resolution as `host --server`: a key with write, else the
			// session's upstream key.
			keys.RyshAPIKey = secret(meeting.RyshAPIKeySecret)
			if keys.RyshAPIKey == "" {
				keys.RyshAPIKey = w.cfg.Upstream.APIKey
			}
		}
		if execute, err = meetingSessionExec(w); err != nil {
			fmt.Fprintf(out, "\n[meeting] %v\n", err)
			w.failRysh("%v", err)
			return
		}
		c.Model = w.sessionModelLabel()
		c.AuditPath = filepath.Join(w.cfg.SessionDir, w.cfg.SessionName, "meeting-audit.jsonl")
	}
	fmt.Fprintf(out, "\n[meeting] %s: starting a bot for agent %q …\n", a.id, a.agent)
	go func() {
		r := manager.Do(c, keys, execute, agent)
		w.meetingAgentsDone(fx, paneID, a, r)
	}()
}

// meetingEffects are the seams a result goroutine uses, captured when the
// command runs.
type meetingEffects struct {
	notice  func(*WorkspaceActor, string, string)
	setMeta func(*WorkspaceActor, string, string, string)
	every   time.Duration
}

func currentMeetingEffects() meetingEffects {
	return meetingEffects{notice: meetingPaneNotice, setMeta: meetingPaneMeta, every: meetingBotWatch}
}

// meetingAgentsDone mirrors the meeting's agents and bot phase into its pane
// meta, then reports the add/remove in the invoking pane.
func (w *WorkspaceActor) meetingAgentsDone(fx meetingEffects, paneID string, a meetingAgentArgs, r meeting.Reply) {
	var text string
	switch {
	case r.Error != "":
		text = fmt.Sprintf("\n[meeting] %s: %s\n", a.id, r.Error)
	case len(r.Agents) == 0:
		text = fmt.Sprintf("\n[meeting] %s: agent %q removed; no agents left\n", a.id, a.agent)
	default:
		text = fmt.Sprintf("\n[meeting] %s: agents %s (bot %s)\n", a.id, strings.Join(r.Agents, ", "), meetingBotPhase(r))
	}
	if r.Error == "" && r.Notice != "" {
		text += "[meeting] " + r.Notice + "\n"
	}
	// A refused command changed nothing, except a bot start that failed.
	if phase := meetingBotPhase(r); r.PaneID != "" && (r.Error == "" || phase == "failed") {
		fx.setMeta(w, r.PaneID, paneMetaMeetingAgents, strings.Join(r.Agents, ","))
		fx.setMeta(w, r.PaneID, paneMetaMeetingBot, phase)
		fx.setMeta(w, r.PaneID, paneMetaMeetingBots, meetingBotsMeta(r))
		if phase == "joining" {
			go w.watchMeetingBot(w.meeting, r.ID, r.PaneID, fx)
		}
	}
	fx.notice(w, paneID, text)
}

// watchMeetingBot follows a joining bot (at most meetingBotWatchFor) and mirrors
// in_call or failed into the pane meta; it stops silently once the bot is
// gone, whose phase the remove or leave that ended it has written.
func (w *WorkspaceActor) watchMeetingBot(manager *meeting.Manager, id, paneID string, fx meetingEffects) {
	for range int(meetingBotWatchFor / fx.every) {
		time.Sleep(fx.every)
		r := manager.Do(meeting.Command{Action: "status", ID: id}, meeting.Credentials{}, nil)
		if r.PaneID != paneID {
			return // unregistered, or the pane changed
		}
		fx.setMeta(w, paneID, paneMetaMeetingBots, meetingBotsMeta(r))
		switch phase := meetingBotPhase(r); phase {
		case "joining":
		case "in_call", "failed":
			fx.setMeta(w, paneID, paneMetaMeetingBot, phase)
			return
		default:
			return // left or gone: remove/leave already wrote the meta
		}
	}
}

// meetingBotsMeta is the meeting.bots value: each bot's agents and phase,
// "<agent>:<phase>" joined by commas ("" without bots).
func meetingBotsMeta(r meeting.Reply) string {
	var parts []string
	for _, b := range r.Bots {
		phase := meetingBotPhase(b)
		for _, name := range b.Agents {
			parts = append(parts, name+":"+phase)
		}
	}
	return strings.Join(parts, ",")
}

// meetingBotPhase maps a bot reply to the contract's meeting.bot values:
// "", joining, in_call, left, failed.
func meetingBotPhase(r meeting.Reply) string {
	switch r.State {
	case "", "idle":
		return ""
	case "joining", "create_uncertain", "starting":
		return "joining"
	case "stopped":
		return "left"
	case "create_failed", "leave_failed", "failed":
		return "failed"
	}
	return "in_call"
}

// meetingNotice publishes a meeting result that arrives after its command
// returned (a bot start, a remove, a leave, a pane created) into BOTH of the
// pane's buffers, as a `##` command's own output is (ryshOutputSink with
// mirrorToRysh). It used to go to the rysh buffer only, which a pane in
// shell, prompt or chat mode does not show: there the user saw "starting a
// bot …" and never the result, a busy refusal included (E-82).
func meetingNotice(pub *msg.NATSPublisher, paneID, text string) {
	if pub == nil {
		return
	}
	_ = pub.SendPaneOutput(paneID, text)
	_ = pub.SendPaneRyshOutput(paneID, text)
}
