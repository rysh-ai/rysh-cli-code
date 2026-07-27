package actors

import (
	"fmt"
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
	if w.replay == nil {
		fmt.Fprintf(out, "replay: capture is OFF\n")
		fmt.Fprintf(out, "  enable with  [replay] enabled: true  or  RYSH_REPLAY_ENABLED=1\n")
		return
	}
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
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
		if w.replayPlayer != nil && w.replayPlayer.Active() {
			if w.replayPaneID != "" {
				fmt.Fprintf(out, "  playback: RUNNING in replay pane %s — focus it for controls, q or ##replay stop to cancel\n", shortID(w.replayPaneID))
				if st, ok := w.replayPlayer.Status(); ok {
					fmt.Fprintf(out, "  %s\n", replay.FormatBadge(st))
				}
			} else {
				fmt.Fprintf(out, "  playback: RUNNING — ##replay stop to cancel\n")
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
		if w.replayPlayer != nil && w.replayPlayer.Active() {
			w.replayPlayer.Stop()
			fmt.Fprintf(out, "replay: stopping playback\n")
		} else {
			fmt.Fprintf(out, "replay: no playback running\n")
		}
	default:
		fmt.Fprintf(out, "usage: ##replay [status] | ##replay export [--pane <id>] [--out <file>] | ##replay play [--pane <id>] [--from <dur|ts>] [--speed <n|max>] | ##replay stop\n")
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
// is called with the rendered badge ("REPLAY 01:23/04:56 ×2") whenever the
// text changes. The badge has 1-second display resolution, so the text-change
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

// handleReplayPlay implements ##replay play (design 006 §3.2 v2). By default
// it creates a NEW dedicated replay pane — a PaneActor variant that never
// starts a shell/PTY, so it is read-only by construction — and replays the
// recording into it with the original timing. While that pane is focused the
// TUI drives playback controls (space pause, ←/→ seek ±10s, +/- speed, q
// close); closing the pane stops the playback. `--here` keeps the v1 in-pane
// behavior: replay into the invoking pane, no controls, ##replay stop cancels.
// One playback at a time either way (single player slot).
func (w *WorkspaceActor) handleReplayPlay(out *strings.Builder, curPane string, args []string) {
	if curPane == "" {
		fmt.Fprintf(out, "replay: no active pane to play into\n")
		return
	}
	if w.replayPlayer != nil && w.replayPlayer.Active() {
		fmt.Fprintf(out, "replay: a playback is already running — ##replay stop first\n")
		return
	}
	target, fromSpec, speedSpec, here, err := parseReplayPlayArgs(args)
	if err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		fmt.Fprintf(out, "usage: ##replay play [--pane <id>] [--from <duration|timestamp>] [--speed <n|max>] [--here]\n")
		return
	}
	if target == "." || target == "current" {
		target = curPane
	}
	if here {
		w.startReplayHere(out, curPane, target, fromSpec, speedSpec)
		return
	}
	w.startReplayPane(out, curPane, target, fromSpec, speedSpec)
}

// startReplayHere is the v1 in-pane playback (##replay play --here).
func (w *WorkspaceActor) startReplayHere(out *strings.Builder, curPane, target, fromSpec, speedSpec string) {
	steps, err := w.replay.PlaybackPlan(target, fromSpec, speedSpec)
	if err != nil {
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
	w.replayPlayer = replay.StartPlayer(steps, time.After, emit, onDone)
	w.replayPaneID = "" // in-pane playback has no dedicated pane to control

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
	tl, err := w.replay.Timeline(target)
	if err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}
	from, err := w.replay.ParseFromOffset(target, fromSpec)
	if err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}
	if len(tl) > 0 && from > tl[len(tl)-1].At {
		fmt.Fprintf(out, "replay: --from %s is past the end of the recording\n", fromSpec)
		return
	}
	speed, err := replay.ParseSpeed(speedSpec)
	if err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}

	tab := w.resolveOriginTab(curPane)
	if tab == nil {
		fmt.Fprintf(out, "replay: no active tab\n")
		return
	}
	laneID := w.resolveLaneInTab(tab, "")
	if laneID == "" {
		fmt.Fprintf(out, "replay: no active lane\n")
		return
	}
	if err := w.checkLimits(1); err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}

	alias := w.generateUniqueAlias()
	paneID := uuid.NewString()
	groupID := uuid.NewString()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePaneGroupInLane{
		LaneID:   laneID,
		Title:    alias,
		GroupID:  groupID,
		PaneID:   paneID,
		PaneType: "replay",
	})
	w.resCounts.panes++
	w.replayPaneID = paneID
	w.restoreFocusAfterCreate(curPane)
	w.persistToKV()

	// All hooks run on the player goroutine and touch no actor state — they
	// publish through the thread-safe NATS publisher only.
	pub := w.pub
	emit := replayEmitter(pub, paneID)
	statusSubject := msg.T("pane", paneID, "status")
	onStatus := replayBadgeFeed(func(badge string) {
		_ = pub.Send(statusSubject, &msg.MsgPaneStatusUpdate{Status: badge})
	})
	clear := func() {
		// Backward seek re-renders from the start of the buffer: wipe the
		// pane, then the player re-emits everything up to the target at max
		// speed (documented v2 behavior).
		_ = pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneClearOutput{})
	}
	onDone := func(stopped bool) {
		line, badge := "\n[rysh] replay: playback finished — press q in this pane to close it\n", "REPLAY finished"
		if stopped {
			line, badge = "\n[rysh] replay: playback stopped\n", "REPLAY stopped"
		}
		emit(line)
		_ = pub.Send(statusSubject, &msg.MsgPaneStatusUpdate{Status: badge})
	}
	w.replayPlayer = replay.StartControlledPlayer(tl, from, speed, replayPaneLead, time.After, replay.PlayerHooks{
		Emit:     emit,
		Clear:    clear,
		OnDone:   onDone,
		OnStatus: onStatus,
	})

	src := "all panes"
	if target != "" {
		src = "pane " + shortID(target)
	}
	fmt.Fprintf(out, "replay: playing %d event(s) from %s into replay pane %q\n", len(tl), src, alias)
	fmt.Fprintf(out, "  focus the pane for controls:  space pause/resume   ←/→ seek ∓10s   +/- speed   q close\n")
	fmt.Fprintf(out, "  ##replay stop cancels from anywhere\n")
}

// replaySeekStep is how far a ←/→ keypress moves the playback position.
const replaySeekStep = 10 * time.Second

// handleReplayControl applies a TUI playback control (design 006 v2). Only
// controls for the active replay pane are honoured — a stale control from a
// closed pane, or one racing a finished playback, is dropped silently.
func (w *WorkspaceActor) handleReplayControl(m *msg.MsgReplayControl) {
	if w.replayPlayer == nil || !w.replayPlayer.Active() {
		return
	}
	if w.replayPaneID == "" || m.PaneID != w.replayPaneID {
		return
	}
	switch m.Action {
	case "pause":
		w.replayPlayer.TogglePause()
	case "seek":
		w.replayPlayer.SeekBy(time.Duration(m.DeltaMs) * time.Millisecond)
	case "faster":
		w.replayPlayer.SpeedUp()
	case "slower":
		w.replayPlayer.SpeedDown()
	case "stop":
		w.replayPlayer.Stop()
	}
}

// stopReplayIfPaneClosed stops the active playback when its dedicated replay
// pane has closed (any close path — the PaneActor's Stopping hook publishes
// MsgPaneStopped for every pane). Returns true when a playback was stopped.
func (w *WorkspaceActor) stopReplayIfPaneClosed(paneID string) bool {
	if paneID == "" || paneID != w.replayPaneID {
		return false
	}
	w.replayPaneID = ""
	if w.replayPlayer != nil && w.replayPlayer.Active() {
		w.replayPlayer.Stop()
		return true
	}
	return false
}
