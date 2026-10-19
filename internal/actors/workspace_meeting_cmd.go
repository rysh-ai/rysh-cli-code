// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##meeting` (design 043 §Command grammar, §Pane contract): a meeting the
// human joins in a shell-less pane of type "meeting" that the desktop app
// renders. The registry (internal/meeting) holds the meeting; the pane is
// born with the meeting.* meta keys below. `add`/`remove agent` live in
// workspace_meeting_agents.go.
//
// Manager.Do can wait behind a bot's provider calls, so nothing here calls it
// on the mailbox: results come back as pane output or as a message to self.

// Pane meta keys set at birth (design 043 §Pane contract).
const (
	paneMetaMeetingID       = "meeting.id"
	paneMetaMeetingKind     = "meeting.kind"
	paneMetaMeetingPlatform = "meeting.platform"
	paneMetaMeetingURL      = "meeting.url"
	paneMetaMeetingProfile  = "meeting.profile"
	paneMetaMeetingRoom     = "meeting.room"
	paneMetaMeetingServer   = "meeting.server"
)

// MeetingServerVar names the session variable that overrides the rysh server
// meeting rooms are opened on; without it the session's upstream.url is used.
const MeetingServerVar = "RYSH_SERVER_URL"

const meetingJoinUsage = "##meeting join <meet/zoom/teams url | rysh:<room> | https://<rysh server>/meet/<room> | new> [--profile <name>] [--title <t>]"

// meetingPaneInfo is what the workspace remembers about a live meeting pane.
type meetingPaneInfo struct {
	id     string // registry id
	origin string // pane that ran ##meeting join ("" after a restore)
}

// msgMeetingPaneCreate asks the workspace (on its mailbox) to create the pane
// of a meeting that has just been registered.
type msgMeetingPaneCreate struct {
	origin string
	paneID string
	reply  meeting.Reply
	meta   map[string]string
	// expires is a new rysh room's end (RFC 3339); then runs once the pane
	// exists (`##meeting new --agent/--invite`, design 047).
	expires string
	then    meetingFollowups
}

// msgMeetingPaneGone re-checks, a moment after a meeting pane's actor
// stopped, whether the pane is really gone (and not, say, moving).
type msgMeetingPaneGone struct{ paneID string }

// msgMeetingClosePane closes a meeting pane on the mailbox (##meeting leave
// resolved its pane off it).
type msgMeetingClosePane struct {
	paneID, origin string
}

// msgMeetingRestore re-registers restored meeting panes.
type msgMeetingRestore struct{}

// meetingPaneGoneDelay is how long after MsgPaneStopped the snapshot is read.
var meetingPaneGoneDelay = 500 * time.Millisecond

// meetingSelf delivers an internal message to this workspace's mailbox. A
// test replaces w.meetingSend to catch it.
func (w *WorkspaceActor) meetingSelf(m any) {
	if w.meetingSend != nil {
		w.meetingSend(m)
		return
	}
	if w.actorSystem != nil && w.selfPID != nil {
		w.actorSystem.Root.Send(w.selfPID, m)
	}
}

func (w *WorkspaceActor) meetingManager() *meeting.Manager {
	if w.meeting == nil {
		w.meeting = meeting.NewManager()
	}
	return w.meeting
}

// meetingServer is the session's rysh server: the RYSH_SERVER_URL session
// variable, else upstream.url.
func (w *WorkspaceActor) meetingServer(paneID string) string {
	if v, ok := w.resolveNamedLayered(w.secretScopeChain(paneID), MeetingServerVar); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return w.cfg.Upstream.URL
}

// meetingRyshKey resolves the rysh key the same way `host --server` does: the
// RYSH_API_KEY secret, else the session's upstream key.
func (w *WorkspaceActor) meetingRyshKey(paneID string) string {
	if w.secrets != nil {
		if v, _, _, _ := w.secrets.GetLayered(w.secretScopeChain(paneID), meeting.RyshAPIKeySecret); v != "" {
			return v
		}
	}
	return w.cfg.Upstream.APIKey
}

func (w *WorkspaceActor) handleMeetingCommand(out *strings.Builder, paneID string, args []string) {
	w.restoreMeetingPanes()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "join":
		w.meetingJoin(out, paneID, args[1:])
	case "new":
		w.meetingNew(out, paneID, args[1:])
	case "invite":
		w.meetingInvite(out, paneID, args[1:])
	case "invites":
		w.meetingInvites(out, paneID, args[1:])
	case "uninvite":
		w.meetingUninvite(out, paneID, args[1:])
	case "", "list":
		if len(args) > 1 {
			w.meetingUsage(out, "##meeting list")
			return
		}
		w.meetingAsync(paneID, meeting.Command{Action: "list"}, sub == "")
	case "status":
		id, ok := w.meetingIDArg(out, paneID, args[1:], "##meeting status [<id>]")
		if ok {
			w.meetingAsync(paneID, meeting.Command{Action: "status", ID: id}, false)
		}
	case "leave":
		id, ok := w.meetingIDArg(out, paneID, args[1:], "##meeting leave [<id>]")
		if ok {
			w.meetingLeave(out, paneID, id)
		}
	case "add", "remove":
		w.meetingAgentCommand(out, paneID, args)
	case "report":
		w.meetingReportCommand(out, paneID, args[1:])
	default:
		ryshWriter(out).Unknown("meeting", sub, meetingNewUsage, "##meeting join <url|rysh:<room>|<server>/meet/<room>|new>",
			"##meeting list | status [<id>] | leave [<id>]", meetingAgentsUsage, meetingInviteUsage, meetingReportUsage)
		w.failRyshUsage("unknown subcommand for ##meeting: %s", sub)
	}
}

var meetingHelp = []string{
	"  ##meeting new [--provider livekit|meet|zoom|teams] [--title t] [--hours n] [--link <url>]\n",
	"                [--agent a]… [--invite <email|guest>]…\n",
	"                                create a meeting (default provider: livekit, or the var MEETING_PROVIDER)\n",
	"  ##meeting join <url|rysh:<room>|<server>/meet/<room>|new> [--profile p] [--title t]\n",
	"                                open a meeting in a new pane (desktop app)\n",
	"  ##meeting invite <id> <email|guest|link> [--name n] [--hours n] | invites <id> | uninvite <id> <invite-id>\n",
	"                                invite people: rysh users or guests without an account\n",
	"  ##meeting list | status [<id>] | leave [<id>]   meetings; leave closes the pane\n",
	"  ##meeting add agent <id> <agent> [--consent] [--public-url <url>] [--allow host,Name,…|anyone] | remove agent <id> <agent>\n",
	"  ##meeting report <id> [status] | on (--consent | --keep-transcript --transcript-consent) [--public-url <url>] | off | now | write\n",
	"                                silent reporter: writes .rysh/meetings/<id>/report.md when the meeting ends\n",
}

func (w *WorkspaceActor) meetingUsage(out *strings.Builder, usage string) {
	ryshWriter(out).UsageLineIn("meeting", usage)
	w.failRyshUsage("usage: %s", usage)
}

// meetingIDArg reads an optional meeting id; without one, the invoking
// pane's own meeting (when run from a meeting pane), else "" = id-less.
func (w *WorkspaceActor) meetingIDArg(out *strings.Builder, paneID string, args []string, usage string) (string, bool) {
	switch {
	case len(args) > 1:
		w.meetingUsage(out, usage)
		return "", false
	case len(args) == 1:
		if !meeting.ValidMeetingID(args[0]) {
			fmt.Fprintf(out, "[meeting] %q is not a meeting id (m-3f9a2c); see ##meeting list\n", args[0])
			w.failRysh("invalid meeting id %q", args[0])
			return "", false
		}
		return args[0], true
	}
	if info, ok := w.meetingPanes[paneID]; ok {
		return info.id, true
	}
	return "", true
}

// meetingAsync runs a read-only registry command off the mailbox and prints
// the result in paneID.
func (w *WorkspaceActor) meetingAsync(paneID string, c meeting.Command, usage bool) {
	manager, notice := w.meetingManager(), meetingPaneNotice
	go func() {
		r := manager.Do(c, meeting.Credentials{}, nil)
		text := formatMeetingReply(c.Action, r)
		if usage {
			text += strings.Join(meetingHelp, "")
		}
		notice(w, paneID, text)
	}()
}

func formatMeetingReply(action string, r meeting.Reply) string {
	var b strings.Builder
	b.WriteString("\n")
	if r.Error != "" {
		fmt.Fprintf(&b, "[meeting] %s\n", r.Error)
		return b.String()
	}
	if action == "list" {
		if len(r.Meetings) == 0 {
			b.WriteString("[meeting] no meetings; open one with ##meeting join <url>\n")
		}
		for _, m := range r.Meetings {
			b.WriteString("[meeting] " + meetingLine(m) + "\n")
		}
		return b.String()
	}
	if r.ID == "" {
		b.WriteString("[meeting] no meetings\n")
		return b.String()
	}
	b.WriteString("[meeting] " + meetingLine(r) + "\n")
	if line := reportLine(r); line != "" {
		b.WriteString("[meeting] " + line + "\n")
	}
	if r.LastReport != "" {
		b.WriteString("[meeting] report: " + r.LastReport + "\n")
	}
	if r.Notice != "" {
		b.WriteString("[meeting] " + r.Notice + "\n")
	}
	return b.String()
}

// meetingLine: id, platform, target, pane, agents, bot state.
func meetingLine(m meeting.Reply) string {
	pane := "no pane"
	if m.PaneID != "" {
		pane = "pane " + shortID(m.PaneID)
	}
	agents := "no agents"
	if len(m.Agents) > 0 {
		agents = "agents " + strings.Join(m.Agents, ",")
	}
	bot := "no bot"
	if phase := meetingBotPhase(m); phase != "" {
		bot = "bot " + phase
	}
	if m.Report != nil {
		bot += ", reporter"
	}
	return fmt.Sprintf("%s  %s  %s  %s  %s  %s", m.ID, m.Platform, m.Target, pane, agents, bot)
}

type meetingJoinArgs struct {
	target, profile, title string
	hours                  int // lifetime of a new rysh room; 0 = 2 h
}

func parseMeetingJoinArgs(args []string) (meetingJoinArgs, error) {
	var a meetingJoinArgs
	usage := fmt.Errorf("usage: %s", meetingJoinUsage)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile", "--title":
			if i+1 >= len(args) || args[i+1] == "" {
				return a, usage
			}
			if args[i] == "--profile" {
				a.profile = args[i+1]
			} else {
				a.title = args[i+1]
			}
			i++
		default:
			if strings.HasPrefix(args[i], "--") || a.target != "" {
				return a, usage
			}
			a.target = args[i]
		}
	}
	if a.target == "" {
		return a, usage
	}
	if len(a.title) > 200 {
		return a, fmt.Errorf("--title: at most 200 characters")
	}
	if a.profile != "" && !validMeetingProfile(a.profile) {
		return a, fmt.Errorf("--profile %q: letters, digits, - and _ only", a.profile)
	}
	return a, nil
}

func validMeetingProfile(p string) bool {
	if len(p) > 64 {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// meetingJoin validates the target on the mailbox, then resolves/creates the
// rysh room (network) and registers the meeting off it; the pane is created
// back on the mailbox by handleMeetingPaneCreate.
func (w *WorkspaceActor) meetingJoin(out *strings.Builder, paneID string, args []string) {
	a, err := parseMeetingJoinArgs(args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage: ") {
			w.meetingUsage(out, meetingJoinUsage)
			return
		}
		fmt.Fprintf(out, "[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	w.meetingOpen(out, paneID, a, meetingFollowups{})
}

// meetingOpen opens a validated join (or new-room) target in a new meeting
// pane; then runs once the pane exists.
func (w *WorkspaceActor) meetingOpen(out *strings.Builder, paneID string, a meetingJoinArgs, then meetingFollowups) {
	fail := func(err error) {
		fmt.Fprintf(out, "[meeting] %v\n", err)
		w.failRysh("%v", err)
	}
	if w.resolveOriginTab(paneID) == nil {
		fail(fmt.Errorf("no active tab"))
		return
	}
	if err := w.checkLimits(1); err != nil {
		fail(err)
		return
	}
	server, key := w.meetingServer(paneID), w.meetingRyshKey(paneID)
	newPane := uuid.NewString()
	c := meeting.Command{Action: "register", PaneID: newPane}
	var resolve func(context.Context) error // network step for rysh rooms
	expires := ""
	switch ref, isRoom, rerr := meeting.ParseRoomRef(a.target, server); {
	case a.target == "new":
		c.Server = server
		ttl := 2 * time.Hour
		if a.hours > 0 {
			ttl = time.Duration(a.hours) * time.Hour
		}
		resolve = func(ctx context.Context) error {
			created, err := meeting.CreateRoom(ctx, server, key, a.title, ttl)
			c.Room, c.Server, expires = created.Room, created.Server, created.ExpiresAt
			return err
		}
	case isRoom:
		if rerr != nil {
			fail(rerr)
			return
		}
		c.Room, c.Server = ref.Room, ref.Server
		resolve = func(ctx context.Context) error { return meeting.CheckRoom(ctx, ref, key) }
	default:
		if _, err := meeting.MeetingPlatform(a.target); err != nil {
			fail(err)
			return
		}
		if a.title != "" {
			fail(fmt.Errorf("--title applies only to ##meeting join new"))
			return
		}
		c.URL = a.target
	}
	if c.URL == "" && a.profile != "" {
		fail(fmt.Errorf("--profile applies only to Meet, Zoom and Teams links"))
		return
	}
	fmt.Fprintf(out, "[meeting] opening %s …\n", a.target)
	manager, notice := w.meetingManager(), meetingPaneNotice
	go func() {
		if resolve != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := resolve(ctx)
			cancel()
			if err != nil {
				notice(w, paneID, fmt.Sprintf("\n[meeting] %v\n", err))
				return
			}
		}
		r := manager.Do(c, meeting.Credentials{}, nil)
		if r.Error != "" {
			notice(w, paneID, fmt.Sprintf("\n[meeting] %s\n", r.Error))
			return
		}
		w.meetingSelf(&msgMeetingPaneCreate{origin: paneID, paneID: newPane, reply: r, meta: meetingBirthMeta(r, c, a.profile), expires: expires, then: then})
	}()
}

// meetingBirthMeta is the pane contract's birth meta for a registered meeting.
func meetingBirthMeta(r meeting.Reply, c meeting.Command, profile string) map[string]string {
	m := map[string]string{
		paneMetaMeetingID:       r.ID,
		paneMetaMeetingKind:     r.Kind,
		paneMetaMeetingPlatform: r.Platform,
	}
	if r.Kind == meeting.KindLiveKit {
		ref := meeting.RoomRef{Server: strings.TrimRight(c.Server, "/"), Room: c.Room}
		m[paneMetaMeetingURL] = ref.MeetLink()
		m[paneMetaMeetingRoom] = ref.Room
		m[paneMetaMeetingServer] = ref.Server
	} else {
		if profile == "" {
			profile = "default"
		}
		m[paneMetaMeetingURL] = c.URL
		m[paneMetaMeetingProfile] = profile
	}
	return m
}

// handleMeetingPaneCreate creates the meeting pane at the bottom of the
// invoking pane's lane, born with its meta; focus stays put.
func (w *WorkspaceActor) handleMeetingPaneCreate(m *msgMeetingPaneCreate) {
	undo := func(why string) {
		manager, notice := w.meetingManager(), meetingPaneNotice
		go func() {
			manager.Do(meeting.Command{Action: "unregister", ID: m.reply.ID}, meeting.Credentials{}, nil)
			notice(w, m.origin, fmt.Sprintf("\n[meeting] %s\n", why))
		}()
	}
	tab := w.resolveOriginTab(m.origin)
	if tab == nil {
		undo("no active tab")
		return
	}
	laneID := w.resolveLaneInTab(tab, "")
	if laneID == "" {
		undo("no active lane")
		return
	}
	if err := w.checkLimits(1); err != nil {
		undo(err.Error())
		return
	}
	alias := w.generateUniqueAlias()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePaneGroupInLane{
		LaneID:   laneID,
		Title:    alias,
		GroupID:  uuid.NewString(),
		PaneID:   m.paneID,
		PaneType: domain.PaneTypeMeeting,
		// Born with its meta: a follow-up MsgPaneSetMeta races the pane
		// actor's subscription (see openAgentsBoardPane).
		Meta: m.meta,
	})
	w.resCounts.panes++
	if w.meetingPanes == nil {
		w.meetingPanes = map[string]meetingPaneInfo{}
	}
	w.meetingPanes[m.paneID] = meetingPaneInfo{id: m.reply.ID, origin: m.origin}
	w.restoreFocusAfterCreate(m.origin)
	w.persistToKV()
	meetingPaneNotice(w, m.origin, fmt.Sprintf("\n[meeting] meeting %s: joined %s in pane %s (%s)\n",
		m.reply.ID, m.reply.Platform, shortID(m.paneID), alias)+meetingCreatedLines(m))
	w.runMeetingFollowups(m.origin, m.reply.ID, m.then)
}

// meetingLeave: a pane meeting leaves by closing its pane (the close path
// unregisters it, so the bot leaves too); a pane-less one by `leave`.
func (w *WorkspaceActor) meetingLeave(out *strings.Builder, paneID, id string) {
	// A pane-less meeting whose bot is starting or leaving refuses the leave
	// (busy refuses, design 043): say so in this command's output (E-82). A
	// pane meeting is not refused — closing its pane stops a starting bot as
	// soon as the start finishes (Manager unregister).
	if id != "" && w.meeting != nil {
		if mt, ok := w.meeting.Lookup(id); ok && mt.PaneID == "" {
			if e := mt.BusyError(); e != "" {
				fmt.Fprintf(out, "[meeting] %s\n", e)
				w.failRysh("%s", e)
				return
			}
		}
	}
	for pane, info := range w.meetingPanes {
		if id != "" && info.id == id {
			fmt.Fprintf(out, "[meeting] meeting %s: leaving, closing pane %s …\n", id, shortID(pane))
			w.closeMeetingPane(pane, paneID)
			return
		}
	}
	manager, notice := w.meetingManager(), meetingPaneNotice
	go func() {
		c := meeting.Command{Action: "status", ID: id}
		r := manager.Do(c, meeting.Credentials{}, nil)
		if r.Error == "" && r.ID != "" && r.PaneID != "" {
			w.meetingSelf(&msgMeetingClosePane{paneID: r.PaneID, origin: paneID})
			return
		}
		if r.Error == "" && r.ID != "" {
			c = meeting.Command{Action: "leave", ID: r.ID}
			r = manager.Do(c, meeting.Credentials{}, nil)
			if r.Error == "" {
				notice(w, paneID, fmt.Sprintf("\n[meeting] meeting %s: left\n", r.ID))
				return
			}
		}
		notice(w, paneID, formatMeetingReply("leave", r))
	}()
}

// closeMeetingPane closes a meeting pane (its group when it is alone there).
func (w *WorkspaceActor) closeMeetingPane(paneID, origin string) {
	if origin != "" {
		if info, ok := w.meetingPanes[paneID]; ok {
			info.origin = origin
			w.meetingPanes[paneID] = info
		}
	}
	for _, t := range w.tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		site, ok := domain.LocatePaneInTab(snap, paneID)
		if !ok {
			continue
		}
		var resp *msg.MsgCLIResponse
		if len(site.Group.Panes) <= 1 {
			resp = w.handleCLIDeletePaneGroup(&msg.MsgCLIDeletePaneGroup{PaneGroupID: site.Group.ID})
		} else {
			resp = w.handleCLIDeletePane(nil, &msg.MsgCLIDeletePane{PaneID: paneID})
		}
		if resp != nil && !resp.OK {
			meetingPaneNotice(w, origin, fmt.Sprintf("\n[meeting] cannot close pane %s: %s\n", shortID(paneID), resp.Error))
		}
		return
	}
	// The pane is already gone: unregister directly.
	w.handleMeetingPaneGone(&msgMeetingPaneGone{paneID: paneID})
}

// meetingPaneStopped: a pane actor stopped. If it was a meeting pane, check
// shortly after whether the pane is really gone.
func (w *WorkspaceActor) meetingPaneStopped(paneID string) {
	if _, ok := w.meetingPanes[paneID]; !ok {
		return
	}
	go func() {
		time.Sleep(meetingPaneGoneDelay)
		w.meetingSelf(&msgMeetingPaneGone{paneID: paneID})
	}()
}

// handleMeetingPaneGone: a meeting pane closed by hand (or by leave) is a
// leave (design 043 D6): the bot leaves and the entry is dropped.
func (w *WorkspaceActor) handleMeetingPaneGone(m *msgMeetingPaneGone) {
	info, ok := w.meetingPanes[m.paneID]
	if !ok || w.paneSnapshotByID(m.paneID) != nil {
		return
	}
	delete(w.meetingPanes, m.paneID)
	manager, notice := w.meetingManager(), meetingPaneNotice
	go func() {
		r := manager.Do(meeting.Command{Action: "unregister", ID: info.id}, meeting.Credentials{}, nil)
		if info.origin == "" {
			return
		}
		text := fmt.Sprintf("\n[meeting] meeting %s: pane closed; left the meeting\n", info.id)
		if r.Error != "" {
			text = fmt.Sprintf("\n[meeting] meeting %s: pane closed, but the bot did not leave: %s (retry: ##meeting leave %s)\n", info.id, r.Error, info.id)
		}
		notice(w, info.origin, text)
	}()
}

// restoreMeetingPanes re-registers meeting panes found in the snapshots
// (after a daemon restart) as bot-less meetings with their ids (D7). It is
// idempotent: panes already known are skipped. The returned channel closes
// once every registration so far (this one and earlier ones) has run.
func (w *WorkspaceActor) restoreMeetingPanes() <-chan struct{} {
	var cmds []meeting.Command
	for _, t := range w.tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		for _, lane := range snap.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if p.PaneType != domain.PaneTypeMeeting {
						continue
					}
					if _, known := w.meetingPanes[p.ID]; known {
						continue
					}
					c, ok := meetingRestoreCommand(p.ID, p.Meta)
					if !ok {
						continue
					}
					if w.meetingPanes == nil {
						w.meetingPanes = map[string]meetingPaneInfo{}
					}
					w.meetingPanes[p.ID] = meetingPaneInfo{id: c.ID}
					cmds = append(cmds, c)
				}
			}
		}
	}
	prev := w.meetingRestored
	if len(cmds) == 0 {
		if prev == nil {
			done := make(chan struct{})
			close(done)
			return done
		}
		return prev
	}
	done := make(chan struct{})
	w.meetingRestored = done
	manager := w.meetingManager()
	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		for _, c := range cmds {
			manager.Do(c, meeting.Credentials{}, nil)
		}
	}()
	return done
}

// meetingRestoreCommand rebuilds the register command from a pane's meta.
func meetingRestoreCommand(paneID string, meta map[string]string) (meeting.Command, bool) {
	c := meeting.Command{Action: "register", ID: meta[paneMetaMeetingID], PaneID: paneID}
	if !meeting.ValidMeetingID(c.ID) {
		return c, false
	}
	switch meta[paneMetaMeetingKind] {
	case meeting.KindWeb:
		c.URL = meta[paneMetaMeetingURL]
	case meeting.KindLiveKit:
		c.Room, c.Server = meta[paneMetaMeetingRoom], meta[paneMetaMeetingServer]
	default:
		return c, false
	}
	return c, true
}

// handleMeetingToken answers the desktop app's meeting_token for a meeting
// pane: a participant token for the room of that pane's registry entry. The
// token goes back to the requester only; it is never logged or stored.
// Without a pane, req.Room names a room on the session's rysh server (the
// app's Meeting panel, E-87): same key, same server, no credential in the app.
func (w *WorkspaceActor) handleMeetingToken(req *msg.MsgMeetingToken, env *msg.RequestEnvelope) {
	if req.PaneID == "" && req.Room != "" {
		w.handleMeetingRoomToken(req.Room, env)
		return
	}
	restored := w.restoreMeetingPanes()
	paneID := req.PaneID
	key := w.meetingRyshKey(paneID)
	manager := w.meetingManager()
	go func() {
		fail := func(e string) { _ = env.Reply(&msg.MsgMeetingTokenReply{PaneID: paneID, Error: e}) }
		<-restored
		mt, ok := manager.ByPane(paneID)
		switch {
		case paneID == "" || !ok:
			fail("not a meeting pane")
			return
		case mt.Kind != meeting.KindLiveKit:
			fail("meeting " + mt.ID + " is not a rysh room; it needs no token")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		g, err := meeting.ParticipantToken(ctx, meeting.RoomRef{Server: mt.Server, Room: mt.Target}, key)
		if err != nil {
			fail(err.Error())
			return
		}
		_ = env.Reply(&msg.MsgMeetingTokenReply{PaneID: paneID, URL: g.URL, Token: g.Token, Identity: g.Identity, ExpiresAt: g.ExpiresAt})
	}()
}

// handleMeetingRoomToken mints a participant token for a room on the
// session's rysh server (RYSH_SERVER_URL, else upstream.url) with the
// session's rysh key; ParticipantToken validates the room id and the server
// before any request.
func (w *WorkspaceActor) handleMeetingRoomToken(room string, env *msg.RequestEnvelope) {
	ref := meeting.RoomRef{Server: w.meetingServer(""), Room: room}
	key := w.meetingRyshKey("")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		g, err := meeting.ParticipantToken(ctx, ref, key)
		if err != nil {
			_ = env.Reply(&msg.MsgMeetingTokenReply{Room: room, Error: err.Error()})
			return
		}
		_ = env.Reply(&msg.MsgMeetingTokenReply{Room: room, URL: g.URL, Token: g.Token, Identity: g.Identity, ExpiresAt: g.ExpiresAt})
	}()
}
