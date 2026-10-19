// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Agent-add flows (design 043 §Agent-add flows, PM-3; D9). `##meeting add
// agent` starts a new bot for the agent — a Recall bot for Meet/Zoom/Teams, a
// LiveKit participant in the existing rysh room — so every agent is its own
// participant, called by its own name. `remove agent` makes that agent's bot
// leave while the meeting (and its pane) stays.

// addAgent implements the "add_agent" verb: c.ID names the meeting and is
// required (an agent is never added to a guessed meeting); agents holds the
// one resolved agent. Called without m.mu; the bot start runs unlocked.
func (m *Manager) addAgent(c Command, keys Credentials, execute Execute, agents []Agent) Reply {
	if len(agents) != 1 {
		return Reply{Error: "add agent needs exactly one resolved agent"}
	}
	name := agents[0].Name
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Reply{Error: "meeting manager shut down"}
	}
	if c.ID == "" {
		r := Reply{Error: "add agent needs a meeting id; meetings: " + m.idsOrNone()}
		m.mu.Unlock()
		return r
	}
	mt, err := m.resolve(c.ID)
	if err != nil {
		m.mu.Unlock()
		return Reply{Error: err.Error()}
	}
	refuse := func(r Reply) Reply {
		r = mt.decorate(r)
		m.mu.Unlock()
		return r
	}
	switch {
	case mt.busy != "":
		return refuse(Reply{Error: busyError(mt)})
	case mt.botOf(name) != nil:
		return refuse(Reply{Error: fmt.Sprintf("agent %q is already in meeting %s", name, mt.ID)})
	case len(mt.Bots) >= MaxAgentBots:
		return refuse(Reply{Error: fmt.Sprintf("meeting %s has %d agent bots, the limit (each transcribes the whole call); remove one first", mt.ID, len(mt.Bots))})
	case mt.hasBot() && c.AllowFromFlag:
		return refuse(Reply{Error: "meeting " + mt.ID + " already has its bots; its allowlist was fixed when the first started (see status). Add the agent without --allow, or leave and add again"})
	}
	if mt.hasBot() {
		// Consent and the allowlist belong to the meeting, fixed by its
		// first bot (D9).
		c.Consent, c.Allow = c.Consent || mt.consent, slices.Clone(mt.allow)
	} else {
		mt.consent, mt.allow = c.Consent, slices.Clone(c.Allow)
	}
	c.Agents, c.Agent = []string{name}, ""
	if agents[0].Voice == "" {
		// Each bot binds one agent, so without this every agent bot would
		// take the provider's first default voice: pick one the meeting's
		// other bots do not speak in.
		agents[0].Voice = meetingVoice(c.TTS, mt.Bots)
	}
	kind, target := mt.Kind, mt.Target
	switch kind {
	case KindWeb:
		// 040's join rules (consent, public URL, keys) are checked by join.
		// Each agent bot is served under its own path of the public URL.
		c.Action, c.URL, c.PublicURL = "join", target, agentPublicURL(c.PublicURL, name)
	case KindLiveKit:
		c.Action, c.Server = "host", mt.Server
	default:
		return refuse(Reply{Error: "meeting " + mt.ID + " has an unknown kind"})
	}
	mt.busy, mt.busyWho = "starting", name
	m.mu.Unlock()
	slot := &botSlot{}
	var r Reply
	if kind == KindWeb {
		r = m.join(slot, c, keys, execute, agents)
	} else {
		r = m.joinServerRoom(slot, c, keys, execute, agents, target)
	}
	r = m.finishStart(mt, false, slot.bot, r, nil)
	if r.Error == "" {
		m.mu.Lock()
		if m.meetings[mt.ID] == mt {
			snap := mt.copy()
			m.mu.Unlock()
			return summarize(snap, r)
		}
		m.mu.Unlock()
	}
	return r
}

// summarize reports the whole meeting after an agent bot changed: the
// agents of every bot, and each bot in Bots; r's notice, if any, wins.
func summarize(snap Meeting, r Reply) Reply {
	info := snap.info()
	if r.Notice != "" {
		info.Notice = r.Notice
	}
	return info
}

// meetingVoice is the first default voice of tts that none of bots' agents
// speaks in ("" when all are taken: bindAgent then picks as before).
func meetingVoice(tts string, bots []*Service) string {
	used := map[string]bool{}
	for _, s := range bots {
		for _, v := range s.voices() {
			used[v] = true
		}
	}
	for _, v := range defaultVoices[tts] {
		if !used[v] {
			return v
		}
	}
	return ""
}

// agentPublicURL is an agent bot's public URL: its own path under the
// session's (D9). An empty base stays empty so join reports it missing.
func agentPublicURL(base, agent string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	return base + "/" + url.PathEscape(agent)
}

// removeAgent implements "remove_agent": the agent's own bot leaves (D9); an
// agent attached to a CLI bot with others is detached from it, and the last
// one out makes that bot leave. The meeting stays registered when it has a
// pane. Called without m.mu.
func (m *Manager) removeAgent(c Command) Reply {
	m.mu.Lock()
	if c.ID == "" {
		r := Reply{Error: "remove agent needs a meeting id; meetings: " + m.idsOrNone()}
		m.mu.Unlock()
		return r
	}
	mt, err := m.resolve(c.ID)
	if err != nil {
		m.mu.Unlock()
		return Reply{Error: err.Error()}
	}
	if mt.busy != "" {
		r := mt.decorate(Reply{Error: busyError(mt)})
		m.mu.Unlock()
		return r
	}
	s := mt.botOf(c.Agent)
	if s == nil {
		r := Reply{Error: fmt.Sprintf("agent %q is not in meeting %s", c.Agent, mt.ID)}
		if !mt.hasBot() {
			r = Reply{State: "idle", Error: fmt.Sprintf("meeting %s has no bot; agent %q is not in it", mt.ID, c.Agent)}
		}
		r = mt.decorate(r)
		m.mu.Unlock()
		return r
	}
	if s.agentCount() > 1 {
		snap := mt.copy()
		m.mu.Unlock()
		return snap.decorate(s.detach(c.Agent))
	}
	left, ok := m.leaveBots(mt, []*Service{s})
	plan, sole := mt.primaryPlan()
	snap := mt.copy()
	m.mu.Unlock()
	applyPrimary(plan, sole)
	if !ok {
		return snap.decorate(left)
	}
	if !snap.hasBot() {
		return snap.decorate(Reply{State: "stopped", Notice: "Last agent removed; the bot left. " + left.Notice})
	}
	return summarize(snap, Reply{Notice: "Agent " + c.Agent + "'s bot left."})
}

func (m *Manager) idsOrNone() string {
	if len(m.order) == 0 {
		return "none"
	}
	return m.ids()
}

// recallBotName is the Recall bot's display name. It keeps the botName
// prefix — the transcript filter drops the bot's own speech by it
// (service.go) — and names the agent that answers (design 043 Decision 3).
func recallBotName(c Command) string {
	if c.report != nil {
		return reporterBotName
	}
	if len(c.Agents) == 1 {
		if name := strings.TrimSpace(c.Agents[0]); name != "" && len(name) <= 40 {
			return botName + " — say hey " + name
		}
	}
	return botName + " — say hey rysh"
}
