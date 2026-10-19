// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/courier"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The fleet actor's watch (design 054): it observes every member, derives
// health, writes what changed to the fleet's board, and checks in with the
// members that need it.
//
// IT TALKS THROUGH THE BOARD AND NOTHING ELSE. A check-in is a board `cmd`
// addressed to the member, so it is delivered exactly as a manager's work order
// is — by the member's own listener (queue) or the courier (grid), beside the
// keyboard and never through it — and it shows up in the board's prompt view
// and flow diagram like any other prompt. An alert is a status post. There is
// no second delivery path to keep honest.
//
// WHAT IT STILL DOES NOT DO: create panes, start agents, or decide work. It
// asks, records and escalates; the parent decides. An actor that reassigned
// work on its own would be a second, invisible manager.

// FleetIdentity is the board identity the fleet actor posts and checks in as.
// Not a pane uuid, like the courier's: the board files a post whose poster does
// not own the thread as a reply, and the flow view draws it as its own node.
const FleetIdentity = "fleet"

// Alert kinds on the board.
const (
	fleetAlertKind = "fleet-alert"
)

// afaWatchInterval is how often members are observed and health derived. The
// check-in INTERVAL is per fleet and much longer; this is only how quickly a
// stall or an unblock is noticed.
var afaWatchInterval = 30 * time.Second

// afaObserveGap is the time between the two screen reads that decide whether a
// pane is working. A working agent repaints its spinner several times a
// second; an idle one does not repaint at all. Width-independent — the footer
// marker vanishes on a 48-column fleet pane (see ansaScreenIsWorking), the
// difference between two frames does not.
var afaObserveGap = 1200 * time.Millisecond

// afaObserveParallel bounds concurrent pane reads.
const afaObserveParallel = 16

// Pane meta keys the watch mirrors, so every surface that already reads pane
// meta — the flow diagram, `##pane info`, fleetctl, the web — sees the ledger
// without a new query. A MIRROR: the registry is the truth, and these are
// written only by this actor.
const (
	MetaFleetWork       = "fleet.work"
	MetaFleetTask       = "fleet.task"
	MetaFleetBlockedBy  = "fleet.blocked_by"
	MetaFleetBlockerFor = "fleet.blocker_for"
	MetaFleetActivity   = "fleet.activity"
	MetaFleetHealth     = "fleet.health"
)

// fleetWatch is the watch's state that is not the ledger: what was mirrored,
// and which fleets were last reported idle.
type fleetWatch struct {
	mu       sync.Mutex
	mirrored map[string]map[string]string // pane -> key -> value
	allIdle  map[string]bool              // fleet -> reported idle
	kick     chan struct{}
	bin      string

	// The watch's clock, snapshotted when the actor is built: its goroutines
	// never read the package variables, so a test tuning them cannot race a
	// fleet actor still winding down from the test before.
	every time.Duration
	gap   time.Duration
}

func newFleetWatch() *fleetWatch {
	bin, _ := os.Executable()
	if bin == "" {
		bin = "rysh"
	}
	return &fleetWatch{
		mirrored: map[string]map[string]string{},
		allIdle:  map[string]bool{},
		kick:     make(chan struct{}, 1),
		bin:      bin,
		every:    afaWatchInterval,
		gap:      afaObserveGap,
	}
}

// watchLoop runs observe → evaluate → check in, on its own goroutine.
func (a *AgentFleetActor) watchLoop(done <-chan struct{}) {
	t := time.NewTicker(a.watch.every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			a.observeOnce()
			a.judgeOnce()
		case <-a.watch.kick:
			// A declaration arrived: re-derive now, so an unblock or a
			// deadlock is acted on in seconds rather than at the next tick.
			a.judgeOnce()
		}
	}
}

// kickWatch asks the loop to re-derive health without waiting for the tick.
func (a *AgentFleetActor) kickWatch() {
	select {
	case a.watch.kick <- struct{}{}:
	default:
	}
}

// observeOnce reads every member's pane and records its activity.
func (a *AgentFleetActor) observeOnce() {
	pub := a.publisher()
	if pub == nil {
		return
	}
	var panes []string
	seen := map[string]bool{}
	for _, f := range a.reg.List() {
		if f.State == fleet.StateDown {
			continue
		}
		for _, m := range f.Members {
			if !seen[m.PaneID] {
				seen[m.PaneID] = true
				panes = append(panes, m.PaneID)
			}
		}
	}
	if len(panes) == 0 {
		return
	}
	results := make([]string, len(panes))
	procs := readProcTable() // once per sweep, for every pane (design 056 §2)
	sem := make(chan struct{}, afaObserveParallel)
	var wg sync.WaitGroup
	for i, id := range panes {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = observePane(pub, id, procs, a.watch.gap)
		}(i, id)
	}
	wg.Wait()
	now := time.Now().UnixMilli()
	for i, id := range panes {
		a.reg.Observe(id, results[i], now)
	}
}

// observePane decides whether one pane's agent is working right now.
//
// Four signals, any one of which means working: a native (rysh) agent mid
// turn; a tool command running under the pane's foreground program (an agent
// waiting on a script it ran — design 056 §2); the footer marker on a pane
// wide enough to show it; or a screen that changed between two reads.
// Unreadable is UNKNOWN, never idle — a loaded session misses VT reads, and
// calling every agent idle exactly when the fleet is busiest would fire a stall
// alert at all of them.
func observePane(pub *msg.NATSPublisher, paneID string, procs *procTable, gap time.Duration) string {
	if inFlight, _, _ := ansaTurnStatus(pub, paneID); inFlight {
		return fleet.ActivityWorking
	}
	if procs != nil {
		if t, err := courier.SnapshotVia(pub, paneID); err == nil && procs.toolRunning(t.PGID) {
			return fleet.ActivityWorking
		}
	}
	tr := &natsAnsaTransport{pub: pub}
	first, err := tr.Screen(paneID)
	if err != nil {
		return fleet.ActivityUnknown
	}
	if ansaScreenIsWorking(first) {
		return fleet.ActivityWorking
	}
	time.Sleep(gap)
	second, err := tr.Screen(paneID)
	if err != nil {
		return fleet.ActivityUnknown
	}
	if ansaScreenIsWorking(second) || strings.Join(first, "\n") != strings.Join(second, "\n") {
		return fleet.ActivityWorking
	}
	return fleet.ActivityIdle
}

// judgeOnce derives health, reports what changed, sends due check-ins,
// mirrors the ledger onto pane meta, and persists.
func (a *AgentFleetActor) judgeOnce() {
	pub := a.publisher()
	now := time.Now().UnixMilli()
	changes := a.reg.Evaluate(now)
	if pub != nil {
		for _, ch := range changes {
			a.reportHealth(pub, ch)
		}
		for _, c := range a.reg.DueCheckins(now) {
			// The status scout answers what it can (design 056): the agent
			// is not woken to say it is still working.
			if f := a.reg.Get(c.Fleet); f != nil && a.tryScout(pub, c, f.Watch.Resolved(), now) {
				continue
			}
			cmdID := a.sendCheckin(pub, c, now)
			if cmdID != "" {
				a.reg.CheckinSent(c.Fleet, c.Member.PaneID, cmdID)
			}
		}
	}
	for _, f := range a.reg.List() {
		if pub != nil {
			a.reportFleetIdle(pub, &f)
			for _, m := range f.Members {
				a.mirror(pub, m)
			}
		}
		a.mu.Lock()
		persist := a.persist
		a.mu.Unlock()
		if persist != nil {
			copyOf := f
			_ = persist.Save(&copyOf)
		}
	}
	// Supervision is reconciled every tick too, so an executor whose fleet
	// changed state through a path that did not call sync is still right
	// within one watch interval.
	a.syncGoalExecutors()
}

// onStatus is called for every accepted declaration: it posts the status on
// the fleet's board AS THE MEMBER (it is the member's own report), on the
// thread of the check-in it answered if there was one, then re-derives.
func (a *AgentFleetActor) onStatus(u fleet.Update, reply fleet.UpdateReply) {
	pub := a.publisher()
	if pub == nil || reply.Fleet == nil || reply.Member == nil || reply.Member.Status == nil {
		a.kickWatch()
		return
	}
	f, m, st := reply.Fleet, reply.Member, reply.Member.Status
	kind := map[string]string{
		fleet.WorkProgress: "progress",
		fleet.WorkBlocked:  msg.BoardKindBlocked,
		fleet.WorkDone:     msg.BoardKindTaskDone,
		fleet.WorkIdle:     "idle",
	}[st.Work]
	var b strings.Builder
	fmt.Fprintf(&b, "status %s", st.Work)
	if st.Task != "" {
		fmt.Fprintf(&b, ": %s", st.Task)
	}
	if len(st.BlockedBy) > 0 {
		fmt.Fprintf(&b, "\nblocked by %s", labels(f, st.BlockedBy))
	}
	if len(st.BlockerFor) > 0 {
		fmt.Fprintf(&b, "\nblocking %s", labels(f, st.BlockerFor))
	}
	if st.Note != "" {
		fmt.Fprintf(&b, "\n%s", st.Note)
	}
	if st.Pct != nil {
		fmt.Fprintf(&b, "\nprogress %d%%", *st.Pct)
	}
	if st.EtaAt > 0 {
		fmt.Fprintf(&b, "\neta %s", time.UnixMilli(st.EtaAt).Format("15:04"))
	}
	p := msg.NewBoardPost(m.PaneID, memberPersona(m), kind, b.String(), time.Now().UnixMilli())
	// Filed on the thread of what it answered — the watch's check-in, else
	// the goal executor's ask — so the board counts that prompt answered.
	switch {
	case reply.AnsweredCheckin != "":
		p.ThreadID = msg.CmdThreadID(FleetIdentity, reply.AnsweredCheckin)
	case reply.AnsweredGoalAsk != "":
		p.ThreadID = msg.CmdThreadID(FleetIdentity, reply.AnsweredGoalAsk)
	}
	if err := msg.SendBoardPost(pub, f.BoardID, p); err != nil {
		slog.Warn("fleet: could not post a status to the board", "fleet", f.Name, "err", err)
	}
	if reply.AnsweredCheckin != "" && reply.AnsweredGoalAsk != "" {
		// One declaration answered both: close the executor's ask on its
		// own thread too, pointing at the status.
		r := msg.NewBoardPost(m.PaneID, memberPersona(m), "progress",
			fmt.Sprintf("answered by my status (%s) on check-in %s", st.Work, reply.AnsweredCheckin), time.Now().UnixMilli())
		r.ThreadID = msg.CmdThreadID(FleetIdentity, reply.AnsweredGoalAsk)
		_ = msg.SendBoardPost(pub, f.BoardID, r)
	}
	a.mirror(pub, *m)
	a.kickWatch()
}

// reportHealth posts one health transition, and escalates the ones a parent
// must act on.
func (a *AgentFleetActor) reportHealth(pub *msg.NATSPublisher, ch fleet.HealthChange) {
	f := a.reg.Get(ch.Fleet)
	if f == nil {
		return
	}
	m, st := ch.Member, ch.Member.Status
	who := memberPersona(&m)
	var text string
	escalate := false
	switch ch.To {
	case fleet.HealthStalled:
		text = fmt.Sprintf("%s is STALLED: says progress (%s) but its pane has been idle since %s",
			who, orNone(st.Task), clock(lastNonZero(st.ActiveAt, st.JoinedAt)))
	case fleet.HealthUnblocked:
		text = fmt.Sprintf("%s is UNBLOCKED: everything it waited on (%s) is resolved", who, labels(f, st.BlockedBy))
	case fleet.HealthDeadlock:
		text = fmt.Sprintf("DEADLOCK: %s — every agent in this cycle waits on the next; none can move", cycleText(f, ch.Cycle))
		escalate = true
	case fleet.HealthUnresponsive:
		text = fmt.Sprintf("%s is UNRESPONSIVE: %d check-ins unanswered", who, st.CheckinMissed)
		escalate = true
	case fleet.HealthAvailable:
		text = fmt.Sprintf("%s is AVAILABLE: it declared %s and its pane is idle — it needs its next work order, or closing",
			who, workText(st))
		escalate = true
	case fleet.HealthSilent:
		text = fmt.Sprintf("%s is SILENT: no status in %s and idle on screen", who, f.Watch.Resolved().IntervalText())
	case fleet.HealthOK:
		switch ch.From {
		case fleet.HealthStalled, fleet.HealthDeadlock, fleet.HealthUnresponsive, fleet.HealthSilent:
			text = fmt.Sprintf("%s recovered (was %s): %s", who, ch.From, workText(st))
		}
	}
	if text == "" {
		return
	}
	alert := msg.NewBoardPost(FleetIdentity, FleetIdentity, fleetAlertKind, text, time.Now().UnixMilli())
	if err := msg.SendBoardPost(pub, f.BoardID, alert); err != nil {
		slog.Warn("fleet: could not post an alert", "fleet", f.Name, "err", err)
	}
	if !escalate || m.Parent == "" {
		return
	}
	// A deadlock is reported once per cycle, to the parent of the cycle's
	// first member, not once per member.
	if ch.To == fleet.HealthDeadlock && len(ch.Cycle) > 0 && ch.Cycle[0] != m.PaneID {
		return
	}
	parent := memberByPane(f, m.Parent)
	if parent == nil {
		return
	}
	a.sendCmd(pub, f, parent, "ESCALATION", text+"\nYou are its parent: act on it (a work order, an answer, or closing it), then declare your own status.")
}

// reportFleetIdle posts once when every member of an up fleet is idle and
// none is blocked on another — the fleet has stopped, and only a human or the
// head can restart it.
func (a *AgentFleetActor) reportFleetIdle(pub *msg.NATSPublisher, f *fleet.Fleet) {
	if f.State != fleet.StateUp || len(f.Members) == 0 {
		return
	}
	idle := true
	for _, m := range f.Members {
		if m.Status == nil || m.Status.Activity != fleet.ActivityIdle || m.Status.Work == fleet.WorkProgress {
			idle = false
			break
		}
	}
	a.watch.mu.Lock()
	was := a.watch.allIdle[f.Name]
	a.watch.allIdle[f.Name] = idle
	a.watch.mu.Unlock()
	if !idle || was {
		return
	}
	text := fmt.Sprintf("FLEET IDLE: all %d members of %s are idle and none says progress — nothing is moving", len(f.Members), f.Name)
	_ = msg.SendBoardPost(pub, f.BoardID, msg.NewBoardPost(FleetIdentity, FleetIdentity, fleetAlertKind, text, time.Now().UnixMilli()))
}

// sendCheckin sends one check-in and returns its cmd id.
func (a *AgentFleetActor) sendCheckin(pub *msg.NATSPublisher, c fleet.Checkin, now int64) string {
	f := a.reg.Get(c.Fleet)
	if f == nil {
		return ""
	}
	m, st := c.Member, c.Member.Status
	w := f.Watch.Resolved()
	var b strings.Builder
	fmt.Fprintf(&b, "Status check (%s). The fleet ledger has you as: %s", c.Reason, workText(st))
	if st.UpdatedAt > 0 {
		fmt.Fprintf(&b, ", declared %s ago", age(now-st.UpdatedAt))
	} else {
		b.WriteString(", never declared")
	}
	if st.Activity == fleet.ActivityIdle {
		fmt.Fprintf(&b, "; your pane has been idle since %s", clock(lastNonZero(st.ActiveAt, st.JoinedAt)))
	}
	b.WriteString(".\n")
	switch c.Reason {
	case fleet.HealthSilent:
		fmt.Fprintf(&b, "You have not declared a status in %s. Declare what you are doing now.\n", w.IntervalText())
	case fleet.HealthStalled:
		b.WriteString("You say progress but nothing is happening in your pane. If you are waiting on someone, declare blocked " +
			"--blocked-by <who>; if you finished, declare done; otherwise resume the task now.\n")
	case fleet.HealthUnblocked:
		fmt.Fprintf(&b, "Everything you were blocked by (%s) is resolved. Resume, and declare progress.\n", labels(f, st.BlockedBy))
	case fleet.HealthUnresponsive:
		fmt.Fprintf(&b, "This is check-in %d with no answer; your parent has been told.\n", c.Missed+1)
	case "scheduled":
		fmt.Fprintf(&b, "Routine check-in (every %s).\n", w.IntervalText())
	}
	fmt.Fprintf(&b, "Answer with ONE command — it records your status and answers this check-in:\n  %s",
		a.statusCommand(f, &m))
	return a.sendCmd(pub, f, &m, "CHECK-IN", b.String())
}

// statusCommand is the exact command a member answers with: binary, session,
// fleet and identity spelled out, for the courier's reason (an agent's shell
// may not carry $RYSH_PANE, and a bare `rysh` may resolve to an older build).
func (a *AgentFleetActor) statusCommand(f *fleet.Fleet, m *fleet.Member) string {
	cmd := fmt.Sprintf("%s fleet status <idle|progress|blocked|done> --as %s --fleet %s",
		shellQuoteArg(a.watch.bin), m.PaneID, f.Name)
	if s := msg.SessionPrefix(); s != "" {
		cmd += " --session " + shellQuoteArg(s)
	}
	return cmd + " --task '<what>' [--blocked-by <who>[,<who>]] [--note '<detail>']"
}

// sendCmd posts a board command from the fleet actor to one member and returns
// its cmd id ("" if it could not be posted).
func (a *AgentFleetActor) sendCmd(pub *msg.NATSPublisher, f *fleet.Fleet, to *fleet.Member, kind, body string) string {
	now := time.Now().UnixMilli()
	cmdID := "ci-" + strings.TrimPrefix(msg.MintCmdID(now), "cmd-")
	text := fmt.Sprintf("[FLEET %s | %s | FROM fleet-actor | TO %s %s (pane %s) | %s]\n%s",
		f.Name, kind, orNone(to.Role), memberPersona(to), shortID(to.PaneID), cmdID, body)
	p := msg.NewBoardPost(FleetIdentity, FleetIdentity, msg.BoardKindCmd, text, now)
	p.ToPaneID, p.ToPersona, p.CmdID = to.PaneID, memberPersona(to), cmdID
	p.ThreadID = msg.CmdThreadID(FleetIdentity, cmdID)
	if err := msg.SendBoardPost(pub, f.BoardID, p); err != nil {
		slog.Warn("fleet: could not post a check-in", "fleet", f.Name, "to", to.Label, "err", err)
		return ""
	}
	return cmdID
}

// mirror writes the member's ledger row onto its pane meta, changed keys only.
func (a *AgentFleetActor) mirror(pub *msg.NATSPublisher, m fleet.Member) {
	st := m.Status
	if st == nil {
		return
	}
	want := map[string]string{
		MetaFleetWork:       st.Work,
		MetaFleetTask:       clip(st.Task, 120),
		MetaFleetBlockedBy:  strings.Join(st.BlockedBy, ","),
		MetaFleetBlockerFor: strings.Join(st.BlockerFor, ","),
		MetaFleetActivity:   st.Activity,
		MetaFleetHealth:     st.Health,
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a.watch.mu.Lock()
	have := a.watch.mirrored[m.PaneID]
	if have == nil {
		have = map[string]string{}
		a.watch.mirrored[m.PaneID] = have
	}
	var send []string
	for _, k := range keys {
		if v, ok := have[k]; ok && v == want[k] {
			continue
		}
		if _, ok := have[k]; !ok && want[k] == "" {
			continue // never written, nothing to clear
		}
		have[k] = want[k]
		send = append(send, k)
	}
	a.watch.mu.Unlock()
	for _, k := range send {
		_ = pub.Send(msg.T("pane", m.PaneID, "inbox"), &msg.MsgPaneSetMeta{Key: k, Value: want[k]})
	}
}

func (a *AgentFleetActor) publisher() *msg.NATSPublisher {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nc == nil || a.stopped {
		return nil
	}
	return msg.NewNATSPublisher(a.nc, a.codecs)
}

func memberByPane(f *fleet.Fleet, paneID string) *fleet.Member {
	for i := range f.Members {
		if f.Members[i].PaneID == paneID {
			return &f.Members[i]
		}
	}
	return nil
}

func memberPersona(m *fleet.Member) string {
	if m.Label != "" {
		return m.Label
	}
	return "pane-" + shortID(m.PaneID)
}

func labels(f *fleet.Fleet, ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fleet.MemberLabel(f, id))
	}
	return strings.Join(out, ", ")
}

func cycleText(f *fleet.Fleet, cyc []string) string {
	if len(cyc) == 0 {
		return "?"
	}
	parts := make([]string, 0, len(cyc)+1)
	for _, id := range cyc {
		parts = append(parts, fleet.MemberLabel(f, id))
	}
	parts = append(parts, fleet.MemberLabel(f, cyc[0]))
	return strings.Join(parts, " → ")
}

func workText(st *fleet.MemberStatus) string {
	if st == nil || st.Work == "" {
		return "no status"
	}
	if st.Task != "" {
		return fmt.Sprintf("%s (%s)", st.Work, clip(st.Task, 80))
	}
	return st.Work
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no task"
	}
	return s
}

func lastNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

func clock(ms int64) string {
	if ms == 0 {
		return "it joined"
	}
	return time.UnixMilli(ms).Format("15:04:05")
}

func age(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// shellQuoteArg quotes s for a POSIX shell only when it needs it.
func shellQuoteArg(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
