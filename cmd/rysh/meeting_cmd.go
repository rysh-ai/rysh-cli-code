// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const meetingUsage = `Google Meet, Zoom and Microsoft Teams via Recall.ai; rysh's own rooms via LiveKit
  rysh meeting doctor [--tts deepgram|elevenlabs|openai] [--stt deepgram|elevenlabs] [--zoom-obf] [--session NAME]
  rysh meeting host [--room NAME] [--server https://RYSH-SERVER] [--tts P] [--stt P] [--lang CODE] [--read-dir /approved/docs]
       [--allow "host,identity,..." | --allow anyone] [--approve [--auto-speak]]
       [--agent NAME,...] [--minutes 60] [--session NAME]
  rysh meeting join MEETING_URL|--id ID --public-url https://HOST [--listen 127.0.0.1:8791] --consent
       [--region us-west-2] [--tts P] [--lang CODE] [--read-dir /approved/docs]
       [--allow "host,Display Name,..." | --allow anyone] [--approve [--auto-speak]]
       [--minutes 60] [--zoom-obf] [--session NAME]
  rysh meeting status|pending|pause|resume|cancel|leave [--id ID] [--session NAME]
  rysh meeting approve|reject|speak TURN_ID [--id ID] [--session NAME]   (--approve mode)
  rysh meeting attach|detach AGENT [--id ID] [--session NAME]
  rysh meeting list [--session NAME]

Meetings have ids (m-3f9a2c): join and host print the new one; list shows all
of them (##meeting join opens one in a pane). Without --id a command acts on
the only meeting with a bot, else the only meeting, else it fails and lists
the ids. join --id ID puts the bot into that pane meeting instead.

MEETING_URL picks the platform:
  https://meet.google.com/abc-defg-hij
  https://zoom.us/j/<id>?pwd=<passcode>   (also <company>.zoom.us, zoom.com, /wc/join/<id>, /my/<name>)
  https://teams.microsoft.com/l/meetup-join/...?context=...   (the "Join the meeting now" link)
  https://teams.microsoft.com/meet/<id>?p=<passcode>, https://teams.live.com/meet/<id>?p=<passcode>
Quote the URL in the shell. Start links, zak/tk tokens, Safe Links and other
redirect or shortened links are refused. The host admits the bot from the
waiting room/lobby; on Zoom the host must also allow its recording request.
--zoom-obf (off by default) is for Zoom meetings hosted by another Zoom account:
Recall fetches an on-behalf-of token from this listener, minted for the Zoom
user in session secrets ZOOM_OAUTH_CLIENT_ID, ZOOM_OAUTH_CLIENT_SECRET and
ZOOM_OAUTH_REFRESH_TOKEN. That user must already be in the meeting.

host creates a room on your LiveKit server (session secrets LIVEKIT_URL,
LIVEKIT_API_KEY, LIVEKIT_API_SECRET; optional LIVEKIT_JOIN_URL for the join
page, default a local LiveKit Meet at http://localhost:3000), puts the bot in
it and prints a join link. No Recall, tunnel or admission. --allow there names
participant identities from the signed join token; the link printed is "host".
With --server, the rysh server creates the room for your account (session
secret RYSH_API_KEY, a rysh key with write; else the session's upstream key),
issues the bot's token and prints an invite: people log in to join, the
allowlist names account ids and "host" is the room owner. The daemon then
needs no LiveKit API secret. Both modes need the --stt provider's key for
listening (DEEPGRAM_API_KEY, or ELEVENLABS_API_KEY with --stt elevenlabs).

Set session secrets RECALL_API_KEY, RECALL_VERIFICATION_SECRET and the --tts
provider's key. Never put keys in command arguments.

Speech providers (design 048): --tts deepgram (default) | elevenlabs | openai
speaks replies (DEEPGRAM_API_KEY, ELEVENLABS_API_KEY, OPENAI_API_KEY); --stt
deepgram (default) | elevenlabs hears a rysh room's participants. Without the
flags the session variables MEETING_TTS / MEETING_STT decide. Recall bots are
heard by Recall's own transcription, so --stt is for host only.
--lang CODE (or MEETING_LANGUAGE), an ISO 639 code such as tr or kmr (Kurmanji):
the agents answer in that language and ElevenLabs speaks it (eleven_v4_turbo);
it needs --tts elevenlabs or openai, and in rysh rooms Kurdish needs --stt
elevenlabs (Deepgram has no Kurdish).
Forward the public HTTPS URL only to the dedicated meeting listener, not the Rysh UI.
Recall processes meeting speech with zero recording retention.

Hands-free by default: an allowed participant says "hey rysh, <request>", the
request runs and the reply is spoken, with no terminal input. --allow picks who
may wake the bot (default: the meeting host); display names are labels, not
authentication. Anyone may say "rysh stop" to cut the current reply. Locally:
cancel drops the current turn, pause stops until resume, leave removes the bot.
--approve restores local approve (and speak, unless --auto-speak) per turn.
Execution uses the session model with no tools except optional scoped file reads.
--json is accepted for every command. No device microphone/camera is used locally.

Agents: spawn one first (##agent spawn scribe), then join with --agent scribe or
attach it to the live meeting (also: ##agent meeting scribe). Say "hey scribe";
the first --agent also answers "hey rysh". An agent speaks in its own voice
(SKILL.md "voice:") and uses ALL its own tools with no approval — only --allow
speakers can address it, "<agent> stop"/pause/leave stop it, and every tool call
is logged (status: tool_calls, audit_log). Up to 4 agents per meeting.
##meeting add agent gives every agent its own bot (up to 4 per meeting).
`

// errMeetingUsage marks a malformed meeting command; runMeetingCmd prints
// meetingUsage to stderr before returning it.
var errMeetingUsage = errors.New("invalid meeting command (usage above)")

func parseMeetingArgs(args []string) (c meeting.Command, sess string, jsonOut bool, err error) {
	if len(args) == 0 {
		return c, sess, false, errMeetingUsage
	}
	c.Action = args[0]
	rest := args[1:]
	rest, sess = extractStringFlag(rest, "--session")
	rest, jsonOut = extractBoolFlag(rest, "--json")
	rest, c.ID = extractStringFlag(rest, "--id")
	if c.ID != "" && !meeting.ValidMeetingID(c.ID) {
		return c, sess, jsonOut, fmt.Errorf("--id %q: meeting ids look like m-3f9a2c", c.ID)
	}
	// Unset speech providers stay unset for the daemon, which fills them
	// from MEETING_TTS / MEETING_STT before its defaults (design 048).
	var typedTTS, typedSTT, typedLang string
	defer func() {
		if c.Action == "join" || c.Action == "host" {
			c.TTS, c.STT, c.Lang = typedTTS, typedSTT, typedLang
		}
	}()
	switch c.Action {
	case "join", "host":
		rest, c.Room = extractStringFlag(rest, "--room")
		rest, c.Server = extractStringFlag(rest, "--server")
		rest, c.PublicURL = extractStringFlag(rest, "--public-url")
		rest, c.Listen = extractStringFlag(rest, "--listen")
		rest, c.Region = extractStringFlag(rest, "--region")
		rest, c.ReadDir = extractStringFlag(rest, "--read-dir")
		rest, c.TTS = extractStringFlag(rest, "--tts")
		rest, c.STT = extractStringFlag(rest, "--stt")
		rest, c.Lang = extractStringFlag(rest, "--lang")
		c.Lang = strings.ToLower(c.Lang)
		typedTTS, typedSTT, typedLang = c.TTS, c.STT, c.Lang
		rest, c.Consent = extractBoolFlag(rest, "--consent")
		rest, c.AutoSpeak = extractBoolFlag(rest, "--auto-speak")
		rest, c.Approve = extractBoolFlag(rest, "--approve")
		rest, c.ZoomOBF = extractBoolFlag(rest, "--zoom-obf")
		var allow string
		rest, allow = extractStringFlag(rest, "--allow")
		if allow != "" {
			for _, a := range strings.Split(allow, ",") {
				c.Allow = append(c.Allow, strings.TrimSpace(a))
			}
		}
		var agents string
		rest, agents = extractStringFlag(rest, "--agent")
		if agents != "" {
			c.Agents = strings.Split(agents, ",")
			if len(c.Agents) > 4 {
				return c, sess, jsonOut, errors.New("--agent takes at most 4 agents")
			}
			for _, a := range c.Agents {
				if err := meeting.ValidAgentName(a); err != nil {
					return c, sess, jsonOut, fmt.Errorf("--agent %q: %v", a, err)
				}
			}
		}
		var mins string
		rest, mins = extractStringFlag(rest, "--minutes")
		if mins != "" {
			n, err := strconv.Atoi(mins)
			if err != nil || n < 1 {
				return c, sess, jsonOut, errors.New("invalid --minutes")
			}
			c.Minutes = n
		}
		if c.Action == "host" {
			if c.ID != "" {
				return c, sess, jsonOut, errors.New("host creates a new meeting; --id is not accepted")
			}
			if len(rest) != 0 {
				return c, sess, jsonOut, errMeetingUsage
			}
			if c.PublicURL != "" || c.Listen != "" || c.Region != "" || c.Consent {
				return c, sess, jsonOut, errors.New("--public-url, --listen, --region and --consent are for join (Recall); host needs none of them")
			}
			c.Defaults()
			if err := c.ValidateHost(); err != nil {
				return c, sess, jsonOut, err
			}
			break
		}
		if c.Room != "" || c.Server != "" {
			return c, sess, jsonOut, errors.New("--room and --server are for host")
		}
		if c.ID != "" && len(rest) == 0 {
			// The pane meeting's own URL; the daemon validates the rest.
			c.Defaults()
			break
		}
		if len(rest) != 1 {
			return c, sess, jsonOut, errMeetingUsage
		}
		c.URL = rest[0]
		c.Defaults()
		if err := c.Validate(); err != nil {
			return c, sess, jsonOut, err
		}
	case "list":
		if len(rest) != 0 || c.ID != "" {
			return c, sess, jsonOut, errMeetingUsage
		}
	case "doctor":
		if c.ID != "" {
			return c, sess, jsonOut, errMeetingUsage
		}
		rest, c.TTS = extractStringFlag(rest, "--tts")
		rest, c.STT = extractStringFlag(rest, "--stt")
		rest, c.ZoomOBF = extractBoolFlag(rest, "--zoom-obf")
		if len(rest) != 0 {
			return c, sess, jsonOut, errMeetingUsage
		}
		if c.TTS != "" && !meeting.ValidTTS(c.TTS) {
			return c, sess, jsonOut, errors.New("invalid --tts (deepgram, elevenlabs or openai)")
		}
		if c.STT != "" && !meeting.ValidSTT(c.STT) {
			return c, sess, jsonOut, errors.New("invalid --stt (deepgram or elevenlabs)")
		}
	case "approve", "reject", "speak":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return c, sess, jsonOut, errMeetingUsage
		}
		c.TurnID = rest[0]
	case "attach", "detach":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return c, sess, jsonOut, errMeetingUsage
		}
		c.Agent = rest[0]
		if err := meeting.ValidAgentName(c.Agent); err != nil {
			return c, sess, jsonOut, err
		}
	case "status", "pending", "pause", "resume", "cancel", "leave":
		if len(rest) != 0 {
			return c, sess, jsonOut, errMeetingUsage
		}
	default:
		return c, sess, jsonOut, errMeetingUsage
	}
	return c, sess, jsonOut, nil
}
func runMeetingCmd(cfg config.Config, args []string) error {
	if len(args) == 1 || args[1] == "help" || args[1] == "--help" || args[1] == "-h" {
		fmt.Print(meetingUsage)
		return nil
	}
	if args[1] == "spike-livekit" { // E29 LK-1 dev harness, in-process
		return runMeetingSpikeLiveKit(cfg, args[2:])
	}
	command, sess, jsonOut, err := parseMeetingArgs(args[1:])
	if errors.Is(err, errMeetingUsage) {
		fmt.Fprint(os.Stderr, meetingUsage)
	}
	if err != nil {
		return err
	}
	if sess == "" {
		sess = os.Getenv("RYSH_SESSION")
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	client, err := cli.Connect(store, sess)
	if err != nil {
		return err
	}
	defer client.Close()
	raw, err := client.Request(&msg.MsgMeetingCommand{Command: command}, 30*time.Second)
	if err != nil {
		return errors.New("meeting command not confirmed; check status before retrying join (daemon may need restarting after upgrade)")
	}
	reply, ok := raw.(*msg.MsgMeetingReply)
	if !ok {
		return errors.New("unexpected meeting reply; update and restart daemon")
	}
	r := reply.Reply
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
			return err
		}
	} else if command.Action == "list" {
		if len(r.Meetings) == 0 {
			fmt.Println("No meetings.")
		}
		for _, m := range r.Meetings {
			fmt.Println(meetingListLine(m))
		}
	} else {
		if r.ID != "" {
			fmt.Println("Meeting id:", r.ID)
		}
		fmt.Println("Meeting:", r.State)
		if r.Platform != "" {
			fmt.Println("Platform:", r.Platform)
		}
		if r.Mode == "approve" {
			fmt.Println("Mode: local approval per turn")
		}
		if r.Room != "" {
			fmt.Println("LiveKit room:", r.Room)
		} else if r.BotID != "" {
			fmt.Println("Recall bot:", r.BotID)
		}
		if r.JoinURL != "" {
			fmt.Println("Join link (valid for this room only; share with participants):", r.JoinURL)
		}
		if r.SegmentsHeard > 0 {
			fmt.Println("Transcript segments heard:", r.SegmentsHeard)
		}
		if len(r.Allow) > 0 {
			label := "display names are labels"
			if r.Platform == meeting.PlatformLiveKit {
				label = "participant identities"
			}
			fmt.Printf("Allowed to wake (%s): %s\n", label, strings.Join(r.Allow, ", "))
		}
		if r.IgnoredWakes > 0 {
			fmt.Println("Wakes ignored (speaker not in --allow):", r.IgnoredWakes)
		}
		if len(r.Agents) > 0 {
			fmt.Println("Agents:", strings.Join(r.Agents, ", "))
		}
		if len(r.Bots) > 1 {
			// One bot per agent (design 043 D9).
			fmt.Printf("Bots (%d):\n", len(r.Bots))
			for _, b := range r.Bots {
				id := b.BotID
				if b.Room != "" {
					id = "LiveKit room " + b.Room
				}
				fmt.Printf("  %s: %s  %s\n", strings.Join(b.Agents, ","), b.State, id)
			}
		}
		if r.TurnID != "" {
			fmt.Println("Turn:", r.TurnID)
		}
		if r.Agent != "" {
			fmt.Println("Addressed agent:", r.Agent)
		}
		if r.Model != "" {
			fmt.Println("Model:", r.Model)
		}
		if r.ToolCalls > 0 {
			fmt.Printf("Agent tool calls: %d (audit log: %s)\n", r.ToolCalls, r.AuditLog)
		}
		if r.Speaker != "" {
			fmt.Printf("Participant label (not authentication): %q\n", r.Speaker)
		}
		if r.Request != "" {
			fmt.Printf("Request: %q\n", r.Request)
		}
		if r.Result != "" {
			fmt.Printf("Reply: %q\n", r.Result)
		}
		if r.Notice != "" {
			fmt.Println(r.Notice)
		}
		if r.Secrets != nil {
			for _, name := range meeting.DoctorSecretNames {
				if present, ok := r.Secrets[name]; ok {
					fmt.Printf("%s configured: %t\n", name, present)
				}
			}
		}
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

// meetingListLine is one `rysh meeting list` row: id, platform, target, pane,
// agents and bot state.
func meetingListLine(m meeting.Reply) string {
	pane := "no pane"
	if m.PaneID != "" {
		pane = "pane " + shortMeetingPane(m.PaneID)
	}
	agents := "no agents"
	if len(m.Agents) > 0 {
		agents = "agents " + strings.Join(m.Agents, ",")
	}
	if len(m.Bots) > 1 {
		agents += fmt.Sprintf(" (%d bots)", len(m.Bots))
	}
	return fmt.Sprintf("%s  %-15s  %s  %s  %s  %s", m.ID, m.Platform, m.Target, pane, agents, m.State)
}

func shortMeetingPane(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
