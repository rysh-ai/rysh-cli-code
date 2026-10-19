// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
)

// `##meeting new`, `invite`, `invites`, `uninvite` (design 047): create a
// meeting — LiveKit unless a provider is named — and invite people to it,
// rysh users or guests without an account. `##rysh meeting …` is an alias.

const (
	meetingNewUsage    = "##meeting new [--provider livekit|meet|zoom|teams] [--title <t>] [--hours <1-72>] [--link <url>] [--profile <p>] [--agent <a>]… [--consent] [--invite <email|guest>]…"
	meetingInviteUsage = "##meeting invite <id> <email|guest|link> [--name <n>] [--hours <n>] | ##meeting invites <id> | ##meeting uninvite <id> <invite-id>"
)

// MeetingProviderVar is the session variable naming the default provider of
// `##meeting new`; unset = livekit.
const MeetingProviderVar = "MEETING_PROVIDER"

// meetingProviders maps what `--provider` accepts to a platform.
var meetingProviders = map[string]string{
	"livekit": meeting.PlatformLiveKit, "rysh": meeting.PlatformLiveKit,
	"meet": meeting.PlatformMeet, "google-meet": meeting.PlatformMeet, "google_meet": meeting.PlatformMeet,
	"zoom":  meeting.PlatformZoom,
	"teams": meeting.PlatformTeams, "microsoft-teams": meeting.PlatformTeams, "microsoft_teams": meeting.PlatformTeams,
}

// meetingCreateHints: where to create a meeting rysh cannot create itself
// (no Google/Zoom/Microsoft integration, design 047 D2).
var meetingCreateHints = map[string]string{
	meeting.PlatformMeet:  "create one at https://meet.new",
	meeting.PlatformZoom:  "create one at https://zoom.us/start/videomeeting",
	meeting.PlatformTeams: "create one in Teams (Calendar → Meet now → copy link)",
}

var meetingPlatformNames = map[string]string{
	meeting.PlatformMeet: "Google Meet", meeting.PlatformZoom: "Zoom", meeting.PlatformTeams: "Microsoft Teams", meeting.PlatformLiveKit: "rysh (LiveKit)",
}

// meetingFollowups run once a new meeting's pane exists.
type meetingFollowups struct {
	agents  []string
	consent bool
	invites []string
}

type meetingNewArgs struct {
	provider, providerFrom string
	platform               string
	join                   meetingJoinArgs
	link                   string
	then                   meetingFollowups
}

// flagValue reads a flag value that may contain spaces (## input is split on
// spaces with no quoting): it runs to the next --flag, and one pair of
// surrounding quotes is dropped. i is left on the value's last word.
func flagValue(args []string, i *int) string {
	var words []string
	for *i+1 < len(args) && !strings.HasPrefix(args[*i+1], "--") {
		*i++
		words = append(words, args[*i])
	}
	v := strings.Join(words, " ")
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return v
}

func parseMeetingNewArgs(args []string) (meetingNewArgs, error) {
	var a meetingNewArgs
	usage := fmt.Errorf("usage: %s", meetingNewUsage)
	one := func(i *int) (string, error) {
		if *i+1 >= len(args) || args[*i+1] == "" || strings.HasPrefix(args[*i+1], "--") {
			return "", usage
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		var err error
		switch args[i] {
		case "--provider":
			a.provider, err = one(&i)
			a.providerFrom = "--provider"
		case "--title":
			a.join.title = flagValue(args, &i)
			if a.join.title == "" {
				err = usage
			}
		case "--hours":
			var v string
			if v, err = one(&i); err == nil {
				a.join.hours, err = strconv.Atoi(v)
				if err != nil || a.join.hours < 1 || a.join.hours > 72 {
					err = fmt.Errorf("--hours: a whole number of hours, 1–72")
				}
			}
		case "--link":
			a.link, err = one(&i)
		case "--profile":
			a.join.profile, err = one(&i)
		case "--agent":
			var v string
			if v, err = one(&i); err == nil {
				a.then.agents = append(a.then.agents, v)
			}
		case "--invite":
			var v string
			if v, err = one(&i); err == nil {
				a.then.invites = append(a.then.invites, v)
			}
		case "--consent":
			a.then.consent = true
		default:
			err = usage
		}
		if err != nil {
			return a, err
		}
	}
	if len(a.join.title) > 200 {
		return a, fmt.Errorf("--title: at most 200 characters")
	}
	if a.join.profile != "" && !validMeetingProfile(a.join.profile) {
		return a, fmt.Errorf("--profile %q: letters, digits, - and _ only", a.join.profile)
	}
	for _, who := range a.then.invites {
		if _, err := parseInviteWho(who); err != nil {
			return a, fmt.Errorf("--invite %s: %v", who, err)
		}
	}
	return a, nil
}

// resolveProvider picks --provider, else MEETING_PROVIDER, else livekit, and
// checks the flags that go with it.
func (a *meetingNewArgs) resolveProvider(fromVar string) error {
	if a.provider == "" && strings.TrimSpace(fromVar) != "" {
		a.provider, a.providerFrom = strings.TrimSpace(fromVar), "the session var "+MeetingProviderVar
	}
	if a.provider == "" {
		a.provider = "livekit"
	}
	platform, ok := meetingProviders[strings.ToLower(a.provider)]
	if !ok {
		return fmt.Errorf("provider %q (from %s): use livekit, meet, zoom or teams", a.provider, a.providerFrom)
	}
	a.platform = platform
	if platform == meeting.PlatformLiveKit {
		switch {
		case a.link != "":
			return fmt.Errorf("--link is for a Meet, Zoom or Teams meeting you created there; a LiveKit room is created here (drop --link, or add --provider meet|zoom|teams)")
		case a.join.profile != "":
			return fmt.Errorf("--profile applies only to Meet, Zoom and Teams meetings")
		}
		a.join.target = "new"
		return nil
	}
	name := meetingPlatformNames[platform]
	switch {
	case a.link == "":
		return fmt.Errorf("rysh cannot create a %s meeting for you (no %s account integration): %s, then run ##meeting new --provider %s --link <url>",
			name, name, meetingCreateHints[platform], strings.ToLower(a.provider))
	case a.join.hours != 0:
		return fmt.Errorf("--hours applies to rysh (LiveKit) rooms; a %s meeting lasts as long as %s keeps it", name, name)
	case len(a.then.invites) > 0:
		return fmt.Errorf("--invite applies to rysh (LiveKit) rooms; invite people to a %s meeting in %s itself", name, name)
	}
	got, err := meeting.MeetingPlatform(a.link)
	if err != nil {
		return err
	}
	if got != platform {
		return fmt.Errorf("--link is a %s link, not %s; use --provider to match it", meetingPlatformNames[got], name)
	}
	a.join.target, a.join.title = a.link, ""
	return nil
}

// meetingNew: `##meeting new` — LiveKit by default (design 047 D1).
func (w *WorkspaceActor) meetingNew(out *strings.Builder, paneID string, args []string) {
	a, err := parseMeetingNewArgs(args)
	if err == nil {
		fromVar := ""
		if w.variables != nil {
			fromVar, _, _, _ = w.variables.GetLayered(w.secretScopeChain(paneID), MeetingProviderVar)
		}
		err = a.resolveProvider(fromVar)
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage: ") {
			w.meetingUsage(out, meetingNewUsage)
			return
		}
		fmt.Fprintf(out, "[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	if r := []rune(a.join.title); len(r) > 120 {
		a.join.title = string(r[:120]) // rysh-server's room-title limit
	}
	w.meetingOpen(out, paneID, a.join, a.then)
}

// meetingCreatedLines follow the "joined" line of a new pane: the room's
// link and end for a rysh room, and what to do next.
func meetingCreatedLines(m *msgMeetingPaneCreate) string {
	var b strings.Builder
	if m.reply.Kind == meeting.KindLiveKit {
		fmt.Fprintf(&b, "[meeting] room link (rysh accounts): %s", m.meta[paneMetaMeetingURL])
		if m.expires != "" {
			fmt.Fprintf(&b, " · ends %s", meetingTime(m.expires))
		}
		b.WriteString("\n")
	}
	if len(m.then.agents) == 0 && len(m.then.invites) == 0 {
		next := "##meeting add agent " + m.reply.ID + " <agent>"
		if m.reply.Kind == meeting.KindLiveKit {
			next += " · ##meeting invite " + m.reply.ID + " <email|guest>"
		}
		fmt.Fprintf(&b, "[meeting] next: %s\n", next)
	}
	return b.String()
}

// meetingTime renders an RFC 3339 time in local time; unparseable input is
// returned as is.
func meetingTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("Mon 2 Jan 15:04")
}

// runMeetingFollowups runs `new --agent/--invite` for a meeting whose pane
// now exists, on the mailbox, exactly as the separate commands would; each
// prints its own result in origin. A failed follow-up leaves the meeting.
func (w *WorkspaceActor) runMeetingFollowups(origin, id string, then meetingFollowups) {
	prior := w.takeRyshFailure() // the command that asked has already answered
	defer func() { w.ryshFail = prior }()
	run := func(f func(*strings.Builder)) {
		var b strings.Builder
		f(&b)
		_ = w.takeRyshFailure()
		if b.Len() > 0 {
			meetingPaneNotice(w, origin, "\n"+strings.TrimLeft(b.String(), "\n"))
		}
	}
	for _, agent := range then.agents {
		args := []string{"add", "agent", id, agent}
		if then.consent {
			args = append(args, "--consent")
		}
		run(func(b *strings.Builder) { w.meetingAgentCommand(b, origin, args) })
	}
	for _, who := range then.invites {
		run(func(b *strings.Builder) { w.meetingInvite(b, origin, []string{id, who}) })
	}
}

// inviteWho is who an invite is for.
type inviteWho struct {
	email       string
	guest, link bool
}

func parseInviteWho(s string) (inviteWho, error) {
	switch {
	case s == "link":
		return inviteWho{link: true}, nil
	case s == "guest":
		return inviteWho{guest: true}, nil
	case strings.Contains(s, "@"):
		if !meeting.ValidInviteEmail(s) {
			return inviteWho{}, fmt.Errorf("%q is not an email address", s)
		}
		return inviteWho{email: s}, nil
	}
	return inviteWho{}, fmt.Errorf("invite whom? an email address (a rysh user or anyone), guest, or link (rysh accounts)")
}

type meetingInviteArgs struct {
	id    string
	who   inviteWho
	name  string
	hours int
}

func parseMeetingInviteArgs(args []string) (meetingInviteArgs, error) {
	var a meetingInviteArgs
	usage := fmt.Errorf("usage: %s", meetingInviteUsage)
	if len(args) < 2 || strings.HasPrefix(args[0], "--") || strings.HasPrefix(args[1], "--") {
		return a, usage
	}
	a.id = args[0]
	if !meeting.ValidMeetingID(a.id) {
		return a, fmt.Errorf("%q is not a meeting id (m-xxxxxx); see ##meeting list", a.id)
	}
	who, err := parseInviteWho(args[1])
	if err != nil {
		return a, err
	}
	a.who = who
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--name":
			a.name = flagValue(args, &i)
			if a.name == "" {
				return a, usage
			}
		case "--hours":
			if i+1 >= len(args) {
				return a, usage
			}
			i++
			h, err := strconv.Atoi(args[i])
			if err != nil || h < 1 || h > 168 {
				return a, fmt.Errorf("--hours: a whole number of hours, 1–168 (never past the room's end)")
			}
			a.hours = h
		default:
			return a, usage
		}
	}
	if a.who.link && a.name != "" {
		return a, fmt.Errorf("--name is for a personal invite (an email or guest), not the shared link")
	}
	if len([]rune(a.name)) > 60 {
		return a, fmt.Errorf("--name: at most 60 characters")
	}
	return a, nil
}

// meetingRoomFor looks a meeting up for invites: a rysh room with a known
// server. For a Meet/Zoom/Teams meeting it prints where invites happen and
// returns false without failing the command.
func (w *WorkspaceActor) meetingRoomFor(out *strings.Builder, id string) (meeting.RoomRef, bool) {
	var mt meeting.Meeting
	ok := false
	if w.meeting != nil {
		mt, ok = w.meeting.Lookup(id)
	}
	switch {
	case !ok:
		err := fmt.Errorf("no meeting %s; see ##meeting list", id)
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
	case mt.Kind == meeting.KindWeb:
		name := meetingPlatformNames[mt.Platform]
		fmt.Fprintf(out, "\n[meeting] %s is a %s meeting: invite people in %s itself (its host admits them); the link is %s\n", id, name, name, mt.Target)
	case mt.Server == "":
		err := fmt.Errorf("meeting %s was not opened on a rysh server (a CLI host without --server); invites need a room from ##meeting new", id)
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
	default:
		return meeting.RoomRef{Server: mt.Server, Room: mt.Target}, true
	}
	return meeting.RoomRef{}, false
}

// meetingInvite: `##meeting invite <id> <email|guest|link>` — the server
// makes the link; rysh prints it and sends nothing (design 047 D5).
func (w *WorkspaceActor) meetingInvite(out *strings.Builder, paneID string, args []string) {
	a, err := parseMeetingInviteArgs(args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage: ") {
			w.meetingUsage(out, meetingInviteUsage)
			return
		}
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	ref, ok := w.meetingRoomFor(out, a.id)
	if !ok {
		return
	}
	req := meeting.InviteRequest{Email: a.who.email, Guest: a.who.guest, Name: a.name, TTL: time.Duration(a.hours) * time.Hour}
	key, notice := w.meetingRyshKey(paneID), meetingPaneNotice
	label := "a guest"
	switch {
	case a.who.email != "":
		label = a.who.email
	case a.who.link:
		label = "rysh accounts"
	}
	fmt.Fprintf(out, "\n[meeting] %s: inviting %s …\n", a.id, label)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		inv, err := meeting.CreateInvite(ctx, ref, key, req)
		if err != nil {
			notice(w, paneID, fmt.Sprintf("\n[meeting] %s: %v\n", a.id, err))
			return
		}
		notice(w, paneID, formatInviteCreated(a.id, label, inv))
	}()
}

func formatInviteCreated(id, label string, inv meeting.Invite) string {
	var b strings.Builder
	if inv.GuestIdentity != "" {
		fmt.Fprintf(&b, "\n[meeting] %s: invite for %s — one person: their rysh account, or a guest without one\n", id, label)
	} else {
		fmt.Fprintf(&b, "\n[meeting] %s: invite link for rysh accounts (up to %d people, sign-in required)\n", id, inv.MaxUses)
	}
	fmt.Fprintf(&b, "[meeting]   %s\n", inv.JoinURL)
	if inv.GuestIdentity != "" {
		fmt.Fprintf(&b, "[meeting]   as a guest they are %s (agents answer them with --allow host,%s or anyone)\n", inv.GuestIdentity, inv.GuestIdentity)
	}
	fmt.Fprintf(&b, "[meeting]   expires %s · rysh sends no email: send the link yourself · revoke: ##meeting uninvite %s %s\n",
		meetingTime(inv.ExpiresAt), id, inv.ID)
	return b.String()
}

// meetingInvites: `##meeting invites <id>`.
func (w *WorkspaceActor) meetingInvites(out *strings.Builder, paneID string, args []string) {
	if len(args) != 1 {
		w.meetingUsage(out, "##meeting invites <id>")
		return
	}
	if !meeting.ValidMeetingID(args[0]) {
		fmt.Fprintf(out, "\n[meeting] %q is not a meeting id (m-3f9a2c); see ##meeting list\n", args[0])
		w.failRysh("invalid meeting id %q", args[0])
		return
	}
	id := args[0]
	ref, ok := w.meetingRoomFor(out, id)
	if !ok {
		return
	}
	key, notice := w.meetingRyshKey(paneID), meetingPaneNotice
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		invites, err := meeting.ListInvites(ctx, ref, key)
		if err != nil {
			notice(w, paneID, fmt.Sprintf("\n[meeting] %s: %v\n", id, err))
			return
		}
		notice(w, paneID, formatInvites(id, invites))
	}()
}

func formatInvites(id string, invites []meeting.Invite) string {
	var b strings.Builder
	if len(invites) == 0 {
		fmt.Fprintf(&b, "\n[meeting] %s: no invites; ##meeting invite %s <email|guest|link>\n", id, id)
		return b.String()
	}
	fmt.Fprintf(&b, "\n[meeting] %s: %d invite(s)\n", id, len(invites))
	for _, inv := range invites {
		who, kind := "rysh accounts", "shared"
		if inv.AllowGuest {
			kind, who = "personal", "a guest"
			if inv.Label != "" {
				who = inv.Label
			}
		}
		state := "expires " + meetingTime(inv.ExpiresAt)
		if inv.RevokedAt != "" {
			state = "revoked"
		}
		used := fmt.Sprintf("%d/%d", inv.Uses, inv.MaxUses)
		if inv.LastUsedAt != "" {
			used += ", guest joined"
		}
		fmt.Fprintf(&b, "[meeting]   %s  %-8s  %s  used %s  %s\n", inv.ID, kind, who, used, state)
	}
	return b.String()
}

// meetingUninvite: `##meeting uninvite <id> <invite-id>`; the server also
// takes that invite's guest out of the room.
func (w *WorkspaceActor) meetingUninvite(out *strings.Builder, paneID string, args []string) {
	if len(args) != 2 {
		w.meetingUsage(out, "##meeting uninvite <id> <invite-id>")
		return
	}
	id, inviteID := args[0], args[1]
	if !meeting.ValidMeetingID(id) || !meeting.ValidInviteID(inviteID) {
		err := fmt.Errorf("uninvite needs a meeting id (m-xxxxxx) and an invite id from ##meeting invites %s", id)
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	ref, ok := w.meetingRoomFor(out, id)
	if !ok {
		return
	}
	key, notice := w.meetingRyshKey(paneID), meetingPaneNotice
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := meeting.RevokeInvite(ctx, ref, key, inviteID); err != nil {
			notice(w, paneID, fmt.Sprintf("\n[meeting] %s: %v\n", id, err))
			return
		}
		notice(w, paneID, fmt.Sprintf("\n[meeting] %s: invite %s revoked; its guest, if in the room, was removed\n", id, inviteID))
	}()
}
