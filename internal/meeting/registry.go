// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Meeting kinds (design 043 §Registry data model).
const (
	KindWeb     = "web"     // Google Meet / Zoom / Teams; its bot is a Recall bot
	KindLiveKit = "livekit" // a LiveKit room; its bot is a LiveKit participant
)

// Meeting is one entry of the session's meeting registry. A pane meeting
// (##meeting join) has a PaneID and no bot until an agent is added; a CLI
// `join`/`host` without --id creates a pane-less entry that holds its bot.
type Meeting struct {
	ID       string // m-xxxxxx
	Kind     string // KindWeb | KindLiveKit
	Platform string // PlatformMeet | PlatformZoom | PlatformTeams | PlatformLiveKit
	Target   string // meeting URL (web) or room id (livekit)
	Server   string // rysh-server base (livekit rooms)
	PaneID   string // "" for pane-less CLI meetings
	// Bots are the meeting's bots in start order: one per agent for
	// `##meeting add agent` (D9), or the one bot of a CLI join/host, which
	// may carry several attached agents.
	Bots      []*Service
	CreatedAt time.Time
	restored  bool // re-registered from pane meta after a daemon restart
	// busy is "starting" or "leaving" while a bot start or leave runs its
	// provider calls outside the registry lock; a second start or leave
	// on the meeting is refused meanwhile. busyWho names the agents the
	// start or leave is for ("" when unknown), for the refusal (E-82).
	busy    string
	busyWho string
	// consent and allow are fixed by the meeting's first agent bot and
	// inherited by every later one (D9).
	consent bool
	allow   []string
	// gone marks a starting meeting that was unregistered: the start
	// makes its new bot leave instead of committing it.
	gone bool
	// Reporter is the meeting's listen-only reporter bot (design 045), kept
	// apart from Bots: it answers no wake, is not an agent and does not
	// count toward MaxAgentBots. lastReport is the newest report written.
	Reporter   *Service
	lastReport string
}

// MaxAgentBots caps the agent bots in one meeting (D9): every bot transcribes
// the whole call, so each one adds a Recall/STT bill.
const MaxAgentBots = 4

func (mt *Meeting) hasBot() bool { return len(mt.Bots) > 0 }

// soleBot is the meeting's bot when it has exactly one, else nil.
func (mt *Meeting) soleBot() *Service {
	if len(mt.Bots) == 1 {
		return mt.Bots[0]
	}
	return nil
}

// botOf is the bot carrying agent, if any.
func (mt *Meeting) botOf(agent string) *Service {
	for _, s := range mt.Bots {
		if s.hasAgent(agent) {
			return s
		}
	}
	return nil
}

func (mt *Meeting) dropBot(s *Service) {
	mt.Bots = slices.DeleteFunc(mt.Bots, func(b *Service) bool { return b == s })
	if mt.Reporter == s {
		mt.Reporter = nil
	}
}

// hasAnyBot: an agent bot or the reporter is in the meeting.
func (mt *Meeting) hasAnyBot() bool { return mt.hasBot() || mt.Reporter != nil }

// allBots are the agent bots and the reporter, which a leave makes go.
func (mt *Meeting) allBots() []*Service {
	all := slices.Clone(mt.Bots)
	if mt.Reporter != nil {
		all = append(all, mt.Reporter)
	}
	return all
}

// copy is a snapshot safe to read outside m.mu: its Bots is its own slice.
func (mt *Meeting) copy() Meeting {
	c := *mt
	c.Bots = slices.Clone(mt.Bots)
	c.allow = slices.Clone(mt.allow)
	return c
}

// primaryPlan says who answers "hey rysh" after the meeting's bots changed:
// the only bot's first agent, or nobody once there are several (D9). The
// caller applies it with applyPrimary after releasing m.mu.
func (mt *Meeting) primaryPlan() (bots []*Service, sole bool) {
	return slices.Clone(mt.Bots), len(mt.Bots) == 1
}

func applyPrimary(bots []*Service, sole bool) {
	for _, s := range bots {
		s.setPrimary(sole)
	}
}

var meetingID = regexp.MustCompile(`^m-[0-9a-f]{6}$`)

// ValidMeetingID reports whether id has the registry's id shape.
func ValidMeetingID(id string) bool { return meetingID.MatchString(id) }

func (m *Manager) newID() (string, error) {
	for range 32 {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return "", errors.New("cannot generate a meeting id")
		}
		id := "m-" + hex.EncodeToString(b)
		_, listed := m.meetings[id]
		if _, starting := m.starting[id]; !listed && !starting {
			return id, nil
		}
	}
	return "", errors.New("cannot generate a unique meeting id")
}

func (m *Manager) add(mt *Meeting) {
	if m.meetings == nil {
		m.meetings = map[string]*Meeting{}
	}
	m.meetings[mt.ID] = mt
	m.order = append(m.order, mt.ID)
}

func (m *Manager) remove(id string) {
	delete(m.meetings, id)
	m.order = slices.DeleteFunc(m.order, func(s string) bool { return s == id })
}

// ids lists the meetings in creation order, with their platform.
func (m *Manager) ids() string {
	var parts []string
	for _, id := range m.order {
		mt := m.meetings[id]
		label := mt.Platform
		switch n := len(mt.Bots); {
		case n == 1:
			label += ", bot"
		case n > 1:
			label += fmt.Sprintf(", %d bots", n)
		}
		parts = append(parts, id+" ("+label+")")
	}
	return strings.Join(parts, ", ")
}

// resolve picks the meeting a command acts on: the named one, else the only
// meeting with a bot, else the only meeting. (nil, nil) means there is none.
func (m *Manager) resolve(id string) (*Meeting, error) {
	if id != "" {
		if mt := m.meetings[id]; mt != nil {
			return mt, nil
		}
		if len(m.meetings) == 0 {
			return nil, fmt.Errorf("no meeting %s; there are no meetings", id)
		}
		return nil, fmt.Errorf("no meeting %s; meetings: %s", id, m.ids())
	}
	if len(m.meetings) == 0 {
		return nil, nil
	}
	var bots []*Meeting
	for _, mid := range m.order {
		if mt := m.meetings[mid]; mt.hasBot() {
			bots = append(bots, mt)
		}
	}
	switch {
	case len(bots) == 1:
		return bots[0], nil
	case len(bots) == 0 && len(m.meetings) == 1:
		return m.meetings[m.order[0]], nil
	}
	return nil, fmt.Errorf("several meetings: %s; name one by id", m.ids())
}

// decorate adds the registry fields to a reply about mt.
func (mt *Meeting) decorate(r Reply) Reply {
	r.ID, r.Kind, r.PaneID, r.Target = mt.ID, mt.Kind, mt.PaneID, mt.Target
	if r.Platform == "" {
		r.Platform = mt.Platform
	}
	if r.Room == "" && mt.Kind == KindLiveKit {
		r.Room = mt.Target
	}
	return r
}

// info is the reply for mt: its bot's status (several bots: an aggregate
// with each one in Bots), or "idle" when it has none. It takes each bot's
// lock, so it runs on a copy outside m.mu when mt has bots.
func (mt *Meeting) info() Reply {
	if mt.hasBot() {
		return mt.withReport(mt.decorate(botsReply(mt.Bots)))
	}
	r := Reply{State: "idle", Notice: "No bot in this meeting."}
	if mt.busy == "starting" {
		r = Reply{State: "starting", Notice: "The bot is starting."}
	} else if mt.restored {
		r.Notice = "Restored after a daemon restart; bots do not survive a restart."
	}
	if mt.Reporter != nil {
		r.Notice = "No agent bot in this meeting; the reporter is taking notes."
	}
	return mt.withReport(mt.decorate(r))
}

// withReport adds the reporter's status and the newest report to r.
func (mt *Meeting) withReport(r Reply) Reply {
	r.LastReport = mt.lastReport
	if mt.Reporter != nil {
		r.Report = mt.Reporter.reportStatus()
	}
	return r
}

// botsReply is the status of a meeting's bots: the bot's own reply when
// there is one (plus Bots), else an aggregate — agents in bot order, the
// least settled state, counters summed — with each bot's reply in Bots.
func botsReply(bots []*Service) Reply {
	each := make([]Reply, 0, len(bots))
	for _, s := range bots {
		each = append(each, s.status(false))
	}
	if len(each) == 1 {
		r := each[0]
		r.Bots = []Reply{each[0]}
		return r
	}
	r := Reply{Bots: each, MediaConnected: true}
	r.State, r.Phase = aggregateState(each)
	var notices []string
	for _, b := range each {
		r.Agents = append(r.Agents, b.Agents...)
		r.Voices = append(r.Voices, b.Voices...)
		r.MediaConnected = r.MediaConnected && b.MediaConnected
		r.SegmentsHeard += b.SegmentsHeard
		r.IgnoredWakes += b.IgnoredWakes
		r.ToolCalls += b.ToolCalls
		if r.Mode == "" {
			r.Mode, r.Allow, r.Platform, r.AuditLog = b.Mode, b.Allow, b.Platform, b.AuditLog
		}
		if b.Notice != "" {
			notices = append(notices, strings.Join(b.Agents, ",")+": "+b.Notice)
		}
	}
	r.Notice = strings.Join(notices, " · ")
	return r
}

// aggregateState is the least settled state among bots: a bot still joining
// makes the meeting joining, then a failed one makes it failed; otherwise the
// first bot still in the call speaks for it, and "stopped" only when all are.
func aggregateState(each []Reply) (state, phase string) {
	rank := func(state string) int {
		switch state {
		case "creating", "joining", "create_uncertain", "starting":
			return 0
		case "create_failed", "leave_failed", "failed":
			return 1
		case "stopped":
			return 3
		}
		return 2
	}
	best, pick := 4, -1
	for i, b := range each {
		if k := rank(b.State); k < best {
			best, pick = k, i
		}
	}
	if pick < 0 {
		return "stopped", ""
	}
	return each[pick].State, each[pick].Phase
}

// snapshot copies the listed meetings in order. Called with m.mu held.
func (m *Manager) snapshot() []Meeting {
	out := make([]Meeting, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.meetings[id].copy())
	}
	return out
}

// list reports meetings from a snapshot; bot status takes each bot's own
// lock, never the registry's.
func list(snaps []Meeting) Reply {
	r := Reply{State: "list", Meetings: []Reply{}}
	for i := range snaps {
		r.Meetings = append(r.Meetings, snaps[i].info())
	}
	return r
}

// register adds a pane meeting with no bot (##meeting join). c.ID is set only
// when a meeting pane is restored with its id; otherwise one is generated.
func (m *Manager) register(c Command) Reply {
	if c.PaneID == "" {
		return Reply{Error: "register needs the meeting pane"}
	}
	for _, mt := range m.meetings {
		if mt.PaneID == c.PaneID {
			return Reply{Error: fmt.Sprintf("pane already holds meeting %s", mt.ID)}
		}
	}
	mt := &Meeting{PaneID: c.PaneID, CreatedAt: time.Now()}
	switch {
	case c.URL != "" && c.Room != "":
		return Reply{Error: "register takes a meeting URL or a room, not both"}
	case c.URL != "":
		platform, err := MeetingPlatform(c.URL)
		if err != nil {
			return Reply{Error: err.Error()}
		}
		mt.Kind, mt.Platform, mt.Target = KindWeb, platform, c.URL
	case c.Room != "":
		if !liveKitRoomName.MatchString(c.Room) {
			return Reply{Error: "invalid room id"}
		}
		base, err := validServerURL(c.Server)
		if err != nil {
			return Reply{Error: err.Error()}
		}
		mt.Kind, mt.Platform, mt.Target, mt.Server = KindLiveKit, PlatformLiveKit, c.Room, base
	default:
		return Reply{Error: "register needs a meeting URL or a room"}
	}
	if c.ID != "" {
		if !ValidMeetingID(c.ID) {
			return Reply{Error: "invalid meeting id " + c.ID}
		}
		if _, taken := m.meetings[c.ID]; taken {
			return Reply{Error: "meeting id " + c.ID + " is already in use"}
		}
		mt.ID, mt.restored = c.ID, true
	} else {
		id, err := m.newID()
		if err != nil {
			return Reply{Error: err.Error()}
		}
		mt.ID = id
	}
	m.add(mt)
	r := mt.info()
	if !mt.restored {
		r.Notice = ""
	}
	return r
}

// startBot runs join/host. With c.ID the bot goes into that (bot-less, web)
// meeting; without it a new pane-less meeting is created to hold the bot and
// kept only if the bot was (or may have been) created. Called without m.mu.
func (m *Manager) startBot(c Command, keys Credentials, execute Execute, agents []Agent) Reply {
	m.mu.Lock()
	mt, fresh, refused := m.reserveStart(&c)
	m.mu.Unlock()
	if refused != nil {
		return *refused
	}
	slot := &botSlot{}
	var r Reply
	if c.Action == "host" {
		r = m.host(slot, c, keys, execute, agents)
	} else {
		r = m.join(slot, c, keys, execute, agents)
	}
	return m.finishStart(mt, fresh, slot.bot, r, func(mt *Meeting) {
		if c.Action == "host" {
			mt.Kind, mt.Platform, mt.Target = KindLiveKit, PlatformLiveKit, r.Room
			mt.Server, _ = validServerURL(c.Server)
		} else {
			mt.Kind, mt.Target = KindWeb, c.URL
			mt.Platform, _ = MeetingPlatform(c.URL)
		}
	})
}

// reserveStart validates a join/host and reserves its meeting (busy
// "starting"; a fresh one goes into m.starting). Called with m.mu held.
func (m *Manager) reserveStart(c *Command) (*Meeting, bool, *Reply) {
	refuse := func(r Reply) (*Meeting, bool, *Reply) { return nil, false, &r }
	if m.closed {
		return refuse(Reply{Error: "meeting manager shut down"})
	}
	var mt *Meeting
	fresh := c.ID == ""
	if fresh {
		id, err := m.newID()
		if err != nil {
			return refuse(Reply{Error: err.Error()})
		}
		mt = &Meeting{ID: id, CreatedAt: time.Now()}
	} else {
		var err error
		if mt, err = m.resolve(c.ID); err != nil {
			return refuse(Reply{Error: err.Error()})
		}
		switch {
		case mt.busy != "":
			return refuse(mt.decorate(Reply{Error: busyError(mt)}))
		case mt.hasBot():
			return refuse(mt.decorate(Reply{Error: "meeting " + mt.ID + " already has a bot; leave it before starting another"}))
		case c.Action == "host":
			return refuse(mt.decorate(Reply{Error: "host creates a new room; meeting " + mt.ID + " already exists"}))
		case mt.Kind != KindWeb:
			return refuse(mt.decorate(Reply{Error: "meeting " + mt.ID + " is a LiveKit room; join takes a Meet, Zoom or Teams meeting"}))
		}
		if c.URL == "" {
			c.URL = mt.Target
		} else if c.URL != mt.Target {
			return refuse(mt.decorate(Reply{Error: "meeting " + mt.ID + " is a different meeting URL"}))
		}
	}
	c.ID = mt.ID // the bot's audit entries carry the meeting id
	who := c.Agents
	if len(who) == 0 && c.Agent != "" {
		who = []string{c.Agent}
	}
	mt.busy, mt.busyWho = "starting", strings.Join(who, ", ")
	if fresh {
		m.starting[mt.ID] = mt
	}
	return mt, fresh, nil
}

// finishStart commits a start: the bot goes into mt (fill completes a fresh
// meeting, which is listed only now). If the meeting was unregistered or the
// manager shut down meanwhile, the new bot leaves at once. Called without
// m.mu.
func (m *Manager) finishStart(mt *Meeting, fresh bool, bot *Service, r Reply, fill func(*Meeting)) Reply {
	m.mu.Lock()
	mt.busy, mt.busyWho = "", ""
	if fresh {
		delete(m.starting, mt.ID)
	}
	abandon := bot != nil && (m.closed || mt.gone)
	var plan []*Service
	var sole bool
	if bot != nil && !abandon {
		mt.Bots = append(mt.Bots, bot)
		if fresh {
			fill(mt)
			m.add(mt)
		}
		plan, sole = mt.primaryPlan()
	}
	if !fresh || mt.hasBot() {
		r = mt.decorate(r)
	}
	m.mu.Unlock()
	applyPrimary(plan, sole)
	if abandon {
		left, ok := leaveBot(bot)
		r.State, r.Error, r.Notice = "stopped", "", "The meeting was closed while its bot was starting; the bot left."
		if !ok {
			r.State, r.Error = left.State, left.Error
			r.Notice = "The meeting was closed while its bot was starting and the bot could not leave; remove it from the provider's dashboard."
		}
	}
	return r
}

// Lookup returns a copy of the meeting with id.
// BusyError is the refusal a start, leave or agent add/remove on this
// meeting gets right now ("" when it is not busy). The grammar layer prints
// it in the invoking pane before it starts anything (E-82); the registry
// still decides, so a start that slips in between is refused the same way.
func (mt *Meeting) BusyError() string {
	if mt.busy == "" {
		return ""
	}
	return busyError(mt)
}

func (m *Manager) Lookup(id string) (Meeting, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mt := m.meetings[id]; mt != nil {
		return mt.copy(), true
	}
	return Meeting{}, false
}

// ByPane returns a copy of the meeting shown in paneID.
func (m *Manager) ByPane(paneID string) (Meeting, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mt := range m.meetings {
		if paneID != "" && mt.PaneID == paneID {
			return mt.copy(), true
		}
	}
	return Meeting{}, false
}

// active is the bot id-less commands act on (nil if none, or if the
// meeting has several); for tests.
func (m *Manager) active() *Service {
	m.mu.Lock()
	defer m.mu.Unlock()
	mt, _ := m.resolve("")
	if mt == nil {
		return nil
	}
	return mt.soleBot()
}
