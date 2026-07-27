package actors

import (
	"fmt"
	"strings"
	"time"

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
			fmt.Fprintf(out, "  playback: RUNNING — ##replay stop to cancel\n")
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
// silently ignored — a mistyped --speed must not play at 1x.
func parseReplayPlayArgs(args []string) (pane, from, speed string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pane":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--pane needs a value")
			}
			i++
			pane = args[i]
		case "--from":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--from needs a value")
			}
			i++
			from = args[i]
		case "--speed":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--speed needs a value")
			}
			i++
			speed = args[i]
		default:
			return "", "", "", fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return pane, from, speed, nil
}

// handleReplayPlay implements ##replay play (design 006 §3.2, in-pane v1):
// re-emit the recorded output into the invoking pane with original timing,
// scaled by --speed and seekable with --from. The replayed bytes travel the
// normal pane output path as system messages tagged replay.RoleReplay —
// playback is display-only by construction (nothing touches the shell's
// stdin/PTY) and the capture skips the tag, so a replay never records itself.
// This is not yet the design's dedicated read-only replay pane (no scrub UI,
// one playback at a time); ##replay stop cancels.
func (w *WorkspaceActor) handleReplayPlay(out *strings.Builder, curPane string, args []string) {
	if curPane == "" {
		fmt.Fprintf(out, "replay: no active pane to play into\n")
		return
	}
	if w.replayPlayer != nil && w.replayPlayer.Active() {
		fmt.Fprintf(out, "replay: a playback is already running — ##replay stop first\n")
		return
	}
	target, fromSpec, speedSpec, err := parseReplayPlayArgs(args)
	if err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		fmt.Fprintf(out, "usage: ##replay play [--pane <id>] [--from <duration|timestamp>] [--speed <n|max>]\n")
		return
	}
	if target == "." || target == "current" {
		target = curPane
	}
	steps, err := w.replay.PlaybackPlan(target, fromSpec, speedSpec)
	if err != nil {
		fmt.Fprintf(out, "replay: %v\n", err)
		return
	}

	// The player goroutine owns no actor state: it publishes through the
	// thread-safe NATS publisher only. Replayed output goes out as ConvShell
	// (dual-published to the merged output subject the pane renders), tagged
	// RoleReplay so the capture ignores it.
	pub := w.pub
	dest := curPane
	emit := func(text string) {
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
	onDone := func(stopped bool) {
		if stopped {
			_ = pub.SendPaneRyshOutput(dest, "\n[rysh] replay: playback stopped\n")
		} else {
			_ = pub.SendPaneRyshOutput(dest, "\n[rysh] replay: playback finished\n")
		}
	}
	w.replayPlayer = replay.StartPlayer(steps, time.After, emit, onDone)

	src := "all panes"
	if target != "" {
		src = "pane " + shortID(target)
	}
	fmt.Fprintf(out, "replay: playing %d event(s) from %s (##replay stop to cancel)\n", len(steps), src)
}
