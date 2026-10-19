// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/replay"
)

// handleReplayCommand implements ##replay (design 006):
//
//	##replay status                     capture state + event counts
//	##replay export [--pane <id>] [--out <file>]
//	                                    write an asciicast (.cast) of captured output
//	##replay play [--pane <id>] [--from <dur|ts>] [--speed <n|max>]
//	                                    replay captured output into this pane, original timing
//	##replay stop                       cancel an in-progress replay
func (w *WorkspaceActor) handleReplayCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	if w.replay == nil {
		fmt.Fprintf(out, "replay: capture is OFF\n")
		fmt.Fprintf(out, "  enable with  [replay] enabled: true  or  RYSH_REPLAY_ENABLED=1\n")
		// Asking for status and being told "OFF" is an answer, not a failure.
		// Asking to export/play/stop and getting it is a refusal — the sub is
		// resolved before this guard so the two can be told apart.
		switch sub {
		case "", "status":
		default:
			w.failRysh("replay capture is OFF — enable it with [replay] enabled: true or RYSH_REPLAY_ENABLED=1")
		}
		return
	}
	switch sub {
	case "", "status":
		panes := w.replay.Panes()
		fmt.Fprintf(out, "replay: capture ON — %d event(s) across %d pane(s)\n", w.replay.Count(""), len(panes))
		if on, msgs := w.replay.DurableInfo(); on {
			fmt.Fprintf(out, "  durable: ON — stream %s, %d message(s)\n", replay.StreamName(w.sessionName), msgs)
		} else {
			fmt.Fprintf(out, "  durable: OFF — in-memory only (recording is lost on restart)\n")
		}
		if w.replayHere != nil && w.replayHere.Active() {
			fmt.Fprintf(out, "  playback: RUNNING in-pane (--here) — ##replay stop to cancel\n")
		}
		for _, id := range w.activeReplayPaneIDs() {
			fmt.Fprintf(out, "  playback: RUNNING in replay pane %s — focus it for controls, q to close it\n", shortID(id))
			if st, ok := w.replayPanes[id].Status(); ok {
				fmt.Fprintf(out, "    %s\n", replay.FormatBadge(st))
			}
		}
		for _, p := range panes {
			fmt.Fprintf(out, "  pane %-10s %d events\n", shortID(p), w.replay.Count(p))
		}
	case "export":
		w.handleReplayExport(out, paneID, args[1:])
	case "play":
		w.handleReplayPlay(out, paneID, args[1:])
	case "stop":
		if n := w.stopAllReplays(); n > 0 {
			fmt.Fprintf(out, "replay: stopping playback (%d)\n", n)
		} else {
			fmt.Fprintf(out, "replay: no playback running\n")
		}
	default:
		fmt.Fprintf(out, "usage: ##replay [status] | ##replay export [--pane <id>] [--out <file>] | ##replay play [--pane <id>] [--from <dur|ts>] [--speed <n|max>] | ##replay stop\n")
		w.failRyshUsage("unknown ##replay subcommand: %q", sub)
	}
}

func (w *WorkspaceActor) handleReplayExport(out *strings.Builder, curPane string, args []string) {
	target := ""  // "" = all panes
	outPath := "" // default derived below
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pane":
			if i+1 < len(args) {
				i++
				target = args[i]
				if target == "." || target == "current" {
					target = curPane
				}
			}
		case "--out", "-o":
			if i+1 < len(args) {
				i++
				outPath = args[i]
			}
		}
	}
	if outPath == "" {
		name := w.sessionName
		if target != "" {
			name += "-" + shortID(target)
		}
		outPath = name + ".cast"
	}
	title := "rysh session " + w.sessionName
	n, err := w.replay.ExportToFile(target, outPath, title)
	if err != nil {
		fmt.Fprintf(out, "replay: export failed: %v\n", err)
		w.failRysh("replay: export failed: %v", err)
		return
	}
	fmt.Fprintf(out, "replay: wrote %d event(s) → %s\n", n, outPath)
	fmt.Fprintf(out, "  play with:  asciinema play %s\n", outPath)
}

// parseReplayPlayArgs parses `##replay play` flags. Pure, so the flag surface
// is unit-testable without an actor. Unknown flags are errors rather than
// silently ignored — a mistyped --speed must not play at 1x. `here` selects
// the v1 in-pane playback (default is a dedicated read-only replay pane).
func parseReplayPlayArgs(args []string) (pane, from, speed string, here bool, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pane":
			if i+1 >= len(args) {
				return "", "", "", false, fmt.Errorf("--pane needs a value")
			}
			i++
			pane = args[i]
		case "--from":
			if i+1 >= len(args) {
				return "", "", "", false, fmt.Errorf("--from needs a value")
			}
			i++
			from = args[i]
		case "--speed":
			if i+1 >= len(args) {
				return "", "", "", false, fmt.Errorf("--speed needs a value")
			}
			i++
			speed = args[i]
		case "--here", "-here", "here":
			// Same flag spelling tolerance as ##new grid's --here.
			here = true
		default:
			return "", "", "", false, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return pane, from, speed, here, nil
}

// replayPaneLead is the grace wait before a replay pane's first emission: the
// pane is created asynchronously (workspace → tab → lane → group → pane spawn
// + NATS subscribe), so emitting immediately could race the pane's output
// subscription and silently drop the first events.
const replayPaneLead = 500 * time.Millisecond

// replayEmitter returns the playback Emit hook for a destination pane: the
// recorded bytes travel the normal pane output path as system messages tagged
// replay.RoleReplay — display-only by construction (nothing touches any
// shell's stdin/PTY) and skipped by the capture, so a replay never records
// itself. Safe on the player goroutine: publishing is thread-safe NATS.
func replayEmitter(pub *msg.NATSPublisher, dest string) func(string) {
	return func(text string) {
		_ = pub.SendConversation(dest, &msg.ConversationMessage{
			TurnID:           msg.NewTurnID(),
			TurnType:         msg.TurnAnswer,
			ConversationType: msg.ConvShell,
			InputType:        msg.InputShell,
			MessageSource:    msg.SourceSystem,
			Role:             replay.RoleReplay,
			Content:          text,
			TimestampMs:      msg.NowMs(),
		})
	}
}

// replayBadgeFeed returns the OnStatus hook feeding the REPLAY badge: publish
// is called with the rendered badge ("REPLAY ▕███░░░░░░░░░▏ 01:23/04:56 · 14:32:07 ×2")
// whenever the text changes. The badge has 1-second display resolution, so the text-change
// check is also the throttle — at most one publish per second of playback
// position plus one per control action. Pure over `publish`, so the dedupe
// logic is unit-testable; runs entirely on the player goroutine.
func replayBadgeFeed(publish func(string)) func(replay.PlayerStatus) {
	last := ""
	return func(st replay.PlayerStatus) {
		b := replay.FormatBadge(st)
		if b == last {
			return
		}
		last = b
		publish(b)
	}
}

// Replay-pane meta (E-15, design 006): a dedicated replay pane carries its
// playback in its metadata, which rides the pane's KV snapshot — so after a
// daemon restart the workspace can find the pane and resume it
// (restoreReplayPanes). Source "*" means all panes merged.
const (
	replayMetaSource = "replay.source"
	replayMetaSpeed  = "replay.speed"
	replayMetaPosMs  = "replay.pos_ms"
	replayMetaState  = "replay.state"

	replayStatePlaying  = "playing" // includes paused: still a live playback
	replayStateFinished = "finished"
	replayStateStopped  = "stopped"

	replaySourceAll = "*"

	// replayPosPersistStep is how far the position must move before it is
	// written to pane meta again: each write is a synchronous KV put, so a
	// restart resumes at most this far behind instead of costing a write per
	// second of playback.
	replayPosPersistStep = 5 * time.Second
)

// formatReplaySpeed renders a speed for pane meta ("max" for +Inf).
func formatReplaySpeed(v float64) string {
	if math.IsInf(v, 1) {
		return "max"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// replayPositionFeed returns an OnStatus hook that records the playback's
// position and speed in the replay pane's meta via set: the position when it
// has moved replayPosPersistStep or more since the last write, and on every
// pause/resume or speed change; the speed when it changes. Pure over set, so
// the throttle is unit-testable; runs on the player goroutine.
func replayPositionFeed(set func(key, value string)) func(replay.PlayerStatus) {
	var last replay.PlayerStatus
	first := true
	return func(st replay.PlayerStatus) {
		speedChanged := first || st.Speed != last.Speed
		moved := st.Pos - last.Pos
		if moved < 0 {
			moved = -moved
		}
		if first || moved >= replayPosPersistStep || st.Paused != last.Paused || speedChanged {
			set(replayMetaPosMs, strconv.FormatInt(st.Pos.Milliseconds(), 10))
			if speedChanged {
				set(replayMetaSpeed, formatReplaySpeed(st.Speed))
			}
			last = st
			first = false
		}
	}
}

// replayPaneHooks builds the controlled player's hooks for a replay pane:
// output, clear, the REPLAY badge, the position/speed meta, and the terminal
// state on finish/stop. Every hook runs on the player goroutine and touches no
// actor state — it publishes through the thread-safe NATS publisher only.
func (w *WorkspaceActor) replayPaneHooks(paneID string) replay.PlayerHooks {
	pub := w.pub
	emit := replayEmitter(pub, paneID)
	statusSubject := msg.T("pane", paneID, "status")
	inbox := msg.T("pane", paneID, "inbox")
	setMeta := func(k, v string) { _ = pub.Send(inbox, &msg.MsgPaneSetMeta{Key: k, Value: v}) }
	badge := replayBadgeFeed(func(b string) {
		_ = pub.Send(statusSubject, &msg.MsgPaneStatusUpdate{Status: b})
	})
	position := replayPositionFeed(setMeta)
	return replay.PlayerHooks{
		Emit: emit,
		Clear: func() {
			// Backward seek / resume re-render from the start of the buffer:
			// wipe the pane, then the player re-emits up to the target.
			_ = pub.Send(inbox, &msg.MsgPaneClearOutput{})
		},
		OnStatus: func(st replay.PlayerStatus) {
			badge(st)
			position(st)
		},
		OnDone: func(stopped bool) {
			line, b, state := "\n[rysh] replay: playback finished — press q in this pane to close it\n", "REPLAY finished", replayStateFinished
			if stopped {
				line, b, state = "\n[rysh] replay: playback stopped\n", "REPLAY stopped", replayStateStopped
			}
			emit(line)
			_ = pub.Send(statusSubject, &msg.MsgPaneStatusUpdate{Status: b})
			setMeta(replayMetaState, state)
		},
	}
}

// startupRestoreDelay is how long after startup restore the workspace waits
// before restoring pane state that needs the tabs to have their panes.
const startupRestoreDelay = 3 * time.Second

// startupRestoreAfter is the clock seam for that wait (time.After in
// production; tests fire it directly instead of sleeping).
var startupRestoreAfter = time.After

// scheduleStartupRestores runs once from Started: after startupRestoreDelay
// it asks the workspace (on its own mailbox) to re-register meeting panes —
// bots do not survive a restart (design 043 D7) — and to resume replay panes
// whose playback the restart cut off (E-15).
func (w *WorkspaceActor) scheduleStartupRestores() {
	wait := startupRestoreAfter(startupRestoreDelay)
	go func() {
		<-wait
		w.meetingSelf(&msgMeetingRestore{})
		w.meetingSelf(&msgReplayRestore{})
	}()
}

// msgReplayRestore is the workspace's note to itself, sent once after startup
// restore, to resume an interrupted replay pane.
type msgReplayRestore struct{}

// restoreReplayPanes resumes a replay pane that was playing when the daemon
// went down (E-15). Its playback is in its meta; the recording comes back
// from the durable capture. It resumes PAUSED at the last saved position with
// the screen re-rendered up to it: nobody was watching across the restart, so
// starting to play unattended would only skip content. Finished or stopped
// replay panes are left as they are; so is a pane that is already playing.
// Every interrupted replay pane is resumed (playbacks are independent).
func (w *WorkspaceActor) restoreReplayPanes() {
	for _, t := range w.tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		for _, lane := range snap.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if p.PaneType != "replay" || p.Meta[replayMetaState] != replayStatePlaying {
						continue
					}
					if cur := w.replayPanes[p.ID]; cur != nil && cur.Active() {
						continue
					}
					w.resumeReplayPane(p.ID, p.Meta)
				}
			}
		}
	}
}

// resumeReplayPane restarts one replay pane's playback from its meta.
func (w *WorkspaceActor) resumeReplayPane(paneID string, meta map[string]string) {
	unavailable := func(why string) {
		_ = w.pub.Send(msg.T("pane", paneID, "status"), &msg.MsgPaneStatusUpdate{Status: "REPLAY unavailable"})
		replayEmitter(w.pub, paneID)("\n[rysh] replay: cannot resume after restart — " + why + "\n")
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetMeta{Key: replayMetaState, Value: replayStateStopped})
	}
	if w.replay == nil {
		unavailable("replay capture is off")
		return
	}
	source := meta[replayMetaSource]
	if source == replaySourceAll {
		source = ""
	}
	tl, start, err := w.replay.ResumeTimeline(source)
	if err != nil {
		unavailable(err.Error())
		return
	}
	speed, err := replay.ParseSpeed(meta[replayMetaSpeed])
	if err != nil {
		speed = 1
	}
	posMs, _ := strconv.ParseInt(meta[replayMetaPosMs], 10, 64)
	w.trackReplayPane(paneID, replay.ResumeControlledPlayer(tl, start, time.Duration(posMs)*time.Millisecond, speed, replayPaneLead, time.After, w.replayPaneHooks(paneID)))
}

// handleReplayPlay implements ##replay play (design 006 §3.2 v2). By default
// it creates a NEW dedicated replay pane — a PaneActor variant that never
// starts a shell/PTY, so it is read-only by construction — and replays the
// recording into it with the original timing. While that pane is focused the
// TUI drives playback controls (space pause, ←/→ seek ±10s, +/- speed, q
// close); closing the pane stops the playback. `--here` keeps the v1 in-pane
// behavior: replay into the invoking pane, no controls, ##replay stop cancels.
// Replay panes play concurrently and independently (one player each); there
// is one in-pane playback at a time.
func (w *WorkspaceActor) handleReplayPlay(out *strings.Builder, curPane string, args []string) {
	if curPane == "" {
		fmt.Fprintf(out, "replay: no active pane to play into\n")
		w.failRysh("replay: no active pane to play into")
		return
	}
	target, fromSpec, speedSpec, here, err := parseReplayPlayArgs(args)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "replay: %v\n", err)
		fmt.Fprintf(out, "usage: ##replay play [--pane <id>] [--from <duration|timestamp>] [--speed <n|max>] [--here]\n")
		return
	}
	if target == "." || target == "current" {
		target = curPane
	}
	if here {
		// Replay panes are independent of each other; an in-pane playback
		// writes into the invoking pane itself, so there is one of those.
		if w.replayHere != nil && w.replayHere.Active() {
			fmt.Fprintf(out, "replay: an in-pane (--here) playback is already running — ##replay stop first\n")
			w.failRysh("replay: an in-pane (--here) playback is already running — ##replay stop first")
			return
		}
		w.startReplayHere(out, curPane, target, fromSpec, speedSpec)
		return
	}
	w.startReplayPane(out, curPane, target, fromSpec, speedSpec)
}

// startReplayHere is the v1 in-pane playback (##replay play --here).
func (w *WorkspaceActor) startReplayHere(out *strings.Builder, curPane, target, fromSpec, speedSpec string) {
	steps, err := w.replay.PlaybackPlan(target, fromSpec, speedSpec)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}

	// The player goroutine owns no actor state: it publishes through the
	// thread-safe NATS publisher only.
	pub := w.pub
	dest := curPane
	emit := replayEmitter(pub, dest)
	onDone := func(stopped bool) {
		if stopped {
			_ = pub.SendPaneRyshOutput(dest, "\n[rysh] replay: playback stopped\n")
		} else {
			_ = pub.SendPaneRyshOutput(dest, "\n[rysh] replay: playback finished\n")
		}
	}
	w.replayHere = replay.StartPlayer(steps, time.After, emit, onDone)

	src := "all panes"
	if target != "" {
		src = "pane " + shortID(target)
	}
	fmt.Fprintf(out, "replay: playing %d event(s) from %s (##replay stop to cancel)\n", len(steps), src)
}

// startReplayPane creates the dedicated read-only replay pane (a new pane
// group at the bottom of the invoking pane's lane, PaneType "replay" — no
// shell ever starts) and schedules a controlled playback into it. Focus stays
// on the invoking pane, like every other pane-creating ## command; focusing
// the replay pane hands its keys to the playback controls.
func (w *WorkspaceActor) startReplayPane(out *strings.Builder, curPane, target, fromSpec, speedSpec string) {
	tl, start, err := w.replay.Timeline(target)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}
	from, err := w.replay.ParseFromOffset(target, fromSpec)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}
	if len(tl) > 0 && from > tl[len(tl)-1].At {
		fmt.Fprintf(out, "replay: --from %s is past the end of the recording\n", fromSpec)
		return
	}
	speed, err := replay.ParseSpeed(speedSpec)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}

	tab := w.resolveOriginTab(curPane)
	if tab == nil {
		fmt.Fprintf(out, "replay: no active tab\n")
		w.failRysh("replay: no active tab")
		return
	}
	laneID := w.resolveLaneInTab(tab, "")
	if laneID == "" {
		fmt.Fprintf(out, "replay: no active lane\n")
		w.failRysh("replay: no active lane")
		return
	}
	if err := w.checkLimits(1); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}

	alias := w.generateUniqueAlias()
	paneID := uuid.NewString()
	groupID := uuid.NewString()
	source := target
	if source == "" {
		source = replaySourceAll
	}
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePaneGroupInLane{
		LaneID:   laneID,
		Title:    alias,
		GroupID:  groupID,
		PaneID:   paneID,
		PaneType: "replay",
		// Born with its playback so a daemon restart can resume it.
		Meta: map[string]string{
			replayMetaSource: source,
			replayMetaSpeed:  formatReplaySpeed(speed),
			replayMetaPosMs:  strconv.FormatInt(from.Milliseconds(), 10),
			replayMetaState:  replayStatePlaying,
		},
	})
	w.resCounts.panes++
	w.restoreFocusAfterCreate(curPane)
	w.persistToKV()

	w.trackReplayPane(paneID, replay.StartControlledPlayer(tl, start, from, speed, replayPaneLead, time.After, w.replayPaneHooks(paneID)))

	src := "all panes"
	if target != "" {
		src = "pane " + shortID(target)
	}
	fmt.Fprintf(out, "replay: playing %d event(s) from %s into replay pane %q\n", len(tl), src, alias)
	fmt.Fprintf(out, "  focus the pane for controls:  space pause/resume   ←/→ seek ∓10s   +/- speed   q close\n")
	fmt.Fprintf(out, "  ##replay stop cancels from anywhere\n")
}

// handleReplayControl applies a TUI playback control (design 006 v2). Only
// controls for the active replay pane are honoured — a stale control from a
// closed pane, or one racing a finished playback, is dropped silently.
func (w *WorkspaceActor) handleReplayControl(m *msg.MsgReplayControl) {
	p := w.replayPanes[m.PaneID]
	if p == nil || !p.Active() {
		return
	}
	switch m.Action {
	case "pause":
		p.TogglePause()
	case "seek":
		p.SeekBy(time.Duration(m.DeltaMs) * time.Millisecond)
	case "faster":
		p.SpeedUp()
	case "slower":
		p.SpeedDown()
	case "stop":
		p.Stop()
	}
}

// stopReplayIfPaneClosed stops the active playback when its dedicated replay
// pane has closed (any close path — the PaneActor's Stopping hook publishes
// MsgPaneStopped for every pane). Returns true when a playback was stopped.
func (w *WorkspaceActor) stopReplayIfPaneClosed(paneID string) bool {
	p, ok := w.replayPanes[paneID]
	if paneID == "" || !ok {
		return false
	}
	delete(w.replayPanes, paneID)
	if p.Active() {
		p.Stop()
		return true
	}
	return false
}

// trackReplayPane records the playback rendering into a replay pane.
func (w *WorkspaceActor) trackReplayPane(paneID string, p *replay.Player) {
	if w.replayPanes == nil {
		w.replayPanes = make(map[string]*replay.Player)
	}
	w.replayPanes[paneID] = p
}

// activeReplayPaneIDs lists the replay panes still playing, sorted.
func (w *WorkspaceActor) activeReplayPaneIDs() []string {
	var ids []string
	for id, p := range w.replayPanes {
		if p.Active() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// stopAllReplays stops every running playback — each replay pane's and the
// in-pane one — and returns how many it stopped (##replay stop, shutdown).
// Replay panes stay open and tracked; their playback ends "stopped".
func (w *WorkspaceActor) stopAllReplays() int {
	n := 0
	for _, p := range w.replayPanes {
		if p.Active() {
			p.Stop()
			n++
		}
	}
	if w.replayHere != nil && w.replayHere.Active() {
		w.replayHere.Stop()
		n++
	}
	return n
}
