package actors

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ---------------------------------------------------------------------------
// Run recording — `##auto web run --record`
//
// WebRecorderActor captures the browser on a fixed interval for the whole run
// and encodes the frames into one video. It drives the browser through the
// SAME NATS subjects the browser_action tool uses
// (`rysh.pane.{id}.browser.request` / `.response`) rather than reaching into
// any particular browser implementation, so one recorder covers both peers
// that serve those subjects: the CLI-owned headless Chromium
// (HeadlessBrowserActor) and the desktop app's embedded browser. Requests
// carry a "rec-" RequestID prefix; browser_action filters responses by its own
// exact RequestID, so the two never intercept each other.
//
// Capture is DROP-don't-queue: if the previous screenshot has not come back
// when the next tick fires, that tick is skipped. A screenshot on a heavy page
// can exceed the interval, and queueing would both drift the recording off
// wall-clock and pile CDP traffic on top of the agent's own browser actions.
// Dropped ticks are why frames carry real elapsed timestamps and the encode
// uses per-frame durations (see buildConcatFile) instead of a fixed fps —
// the resulting video matches real time even when captures were missed.
//
// PRIVACY: recordings are pixels of a logged-in browser. SharedOutputActor
// redacts secrets from text, but it cannot redact an image. Frames and the
// encoded video are therefore written to local disk only and are NEVER
// published to shared output, a share, or upstream. Only the file path is
// reported into the pane.
// ---------------------------------------------------------------------------

// recTick asks for one frame. It carries no sequence: the ticker is stopped by
// closing quit, and the done flag rejects anything still in flight. (An
// earlier version shared one counter with the grace timers below, which meant
// the first run_start event silently invalidated every subsequent tick.)
type recTick struct{}

// recStop ends recording and encodes. Sent by the AutoLoopActor supervisor
// when it owns the run's lifecycle, or by the recorder itself.
type recStop struct{ reason string }

// recGraceElapsed fires when an unsupervised recorder's pause grace window
// expires without the run restarting — the run is over.
type recGraceElapsed struct{ seq int }

// recFrame is one captured frame: its filename and the elapsed time since
// recording started (used to give the encoder real per-frame durations).
type recFrame struct {
	name string
	at   time.Duration
}

// WebRecorderActor records one `##auto web run`. Spawned by the
// WorkspaceActor, it stops itself once encoding finishes.
type WebRecorderActor struct {
	paneID     string
	recipeName string
	spec       webauto.RecordSpec
	outPath    string // absolute video path, resolved by the caller
	framesDir  string // scratch dir holding the frames until encode

	// supervised marks a run whose AutoLoopActor owns the stop signal. A
	// looped run is ONE logical run spanning every pass, so the recorder must
	// not self-stop when an individual pass ends — the supervisor sends
	// recStop when the whole loop finishes. Unsupervised runs have no such
	// signal, so the recorder watches the step stream itself.
	supervised bool

	pub *msg.NATSPublisher
	nc  *nats.Conn
	br  *bridge.NATSBridge

	startedAt time.Time
	quit      chan struct{} // closed on Stopping to end the ticker goroutine

	// graceSeq invalidates stale pause-grace timers ONLY. It must not gate
	// captures: the step stream bumps it on every run restart, and a shared
	// counter would switch recording off the first time that happened.
	graceSeq   int
	reqPrefix  string // "rec-<pane>-<stamp>-", unique per recorder
	reqSeq     int
	inFlight   string    // outstanding capture RequestID ("" = idle)
	inFlightAt time.Time // when it was sent, for the stall guard
	// sentAt records when each issued capture was REQUESTED, keyed by request
	// id. It outlives inFlight on purpose: the stall guard stops us waiting on
	// a slow screenshot, but it must not make us throw the answer away when it
	// does arrive. A frame is stamped with its request time rather than its
	// arrival time, so a late one still sits at the right point in the video.
	sentAt map[string]time.Duration

	frames   []recFrame
	dropped  int // ticks skipped because a capture was still outstanding
	failed   int // captures that came back unsuccessful
	timedOut int // captures the stall guard stopped waiting on (some still arrive)
	done     bool
}

// NewWebRecorderActor builds the recorder. outPath must be absolute and
// already resolved (see webauto.RecordSpec.ResolvePath).
func NewWebRecorderActor(paneID, recipeName string, spec webauto.RecordSpec, outPath string,
	supervised bool, pub *msg.NATSPublisher, nc *nats.Conn) *WebRecorderActor {
	return &WebRecorderActor{
		paneID:     paneID,
		recipeName: recipeName,
		spec:       spec,
		outPath:    outPath,
		supervised: supervised,
		pub:        pub,
		nc:         nc,
	}
}

// Receive implements actor.Actor.
func (r *WebRecorderActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		r.startedAt = time.Now()
		r.reqPrefix = fmt.Sprintf("rec-%s-%d-", r.paneID, r.startedAt.UnixNano())

		// Frames land next to the video so a partial run leaves everything in
		// one place, and the encode never crosses a filesystem boundary.
		r.framesDir = strings.TrimSuffix(r.outPath, filepath.Ext(r.outPath)) + ".frames"
		if err := os.MkdirAll(r.framesDir, 0o755); err != nil {
			_ = r.pub.SendPaneRyshOutput(r.paneID,
				fmt.Sprintf("[web] recording disabled: cannot create %s: %v\n", r.framesDir, err))
			ctx.Stop(ctx.Self())
			return
		}

		r.br = bridge.New(r.nc, ctx.Self(), ctx.ActorSystem(), r.pub.Codecs())
		if err := r.br.AddSubject(msg.T("pane", r.paneID, "browser", "response")); err != nil {
			slog.Error("web-record: subscribe browser.response", "pane", r.paneID, "err", err)
		}
		// Unsupervised runs have no AutoLoopActor to tell us the run ended, so
		// watch the same step stream the supervisor would.
		if !r.supervised {
			if err := r.br.AddSubject(msg.T("pane", r.paneID, "llm_prompt_execution", "steps")); err != nil {
				slog.Error("web-record: subscribe steps", "pane", r.paneID, "err", err)
			}
		}

		r.quit = make(chan struct{})
		r.startTicker(ctx)
		slog.Info("web-record: started", "pane", r.paneID, "recipe", r.recipeName,
			"interval", r.spec.Interval, "out", r.outPath)

	case *actor.Stopping:
		if r.quit != nil {
			close(r.quit)
			r.quit = nil
		}
		if r.br != nil {
			r.br.Stop()
			r.br = nil
		}

	case *recTick:
		r.capture()

	case *msg.MsgBrowserActionResponse:
		r.handleFrame(m)

	case *msg.MsgAgenticStep:
		r.handleStep(ctx, m)

	case *recGraceElapsed:
		if m.seq == r.graceSeq {
			r.finish(ctx, "run ended at its budget")
		}

	case *recStop:
		r.finish(ctx, m.reason)
	}
}

// startTicker runs the capture clock. The goroutine captures only immutable
// values (the PID, the actor system, the interval, the quit channel) per the
// actor rules — all state lives in the mailbox.
func (r *WebRecorderActor) startTicker(ctx actor.Context) {
	self, system, interval, quit := ctx.Self(), ctx.ActorSystem(), r.spec.Interval, r.quit
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				system.Root.Send(self, &recTick{})
			}
		}
	}()
}

// capture requests one frame, unless a capture is already outstanding.
func (r *WebRecorderActor) capture() {
	if r.done {
		return
	}
	if r.inFlight != "" {
		// Stall guard: a peer that never answers (browser died, desktop app
		// detached) would otherwise wedge recording forever. After a few
		// intervals, stop WAITING on that frame so the next tick can try
		// again.
		//
		// Stop waiting, but do not disown it. The id stays in sentAt, so if
		// the screenshot was merely slow rather than lost, handleFrame still
		// keeps it. Clearing inFlight used to be the same thing as discarding
		// the answer, which meant a machine where every screenshot took longer
		// than the guard's window recorded NOTHING — every capture succeeded,
		// every one arrived a moment too late, and the run reported "no frames
		// captured". A busy machine is exactly when a long automation is being
		// recorded, so that was the case most likely to need the video.
		if time.Since(r.inFlightAt) > 4*r.spec.Interval {
			r.inFlight = ""
			r.timedOut++
		}
		r.dropped++
		return
	}
	r.pruneSentAt()
	if r.spec.MaxFrames > 0 && len(r.frames) >= r.spec.MaxFrames {
		return // cap reached; keep the run going, just stop growing the file
	}
	r.reqSeq++
	r.inFlight = r.reqPrefix + fmt.Sprint(r.reqSeq)
	r.inFlightAt = time.Now()
	if r.sentAt == nil {
		r.sentAt = make(map[string]time.Duration, 4)
	}
	r.sentAt[r.inFlight] = time.Since(r.startedAt)

	// settle:false tells the desktop app's executor to skip the
	// requestAnimationFrame + 80ms wait it runs before an agent screenshot.
	// That settle is right for a capture the model will reason about, but a
	// recorder firing every few hundred milliseconds would be injecting JS
	// into the page under automation for the whole run and paying the delay on
	// every frame. A torn frame costs nothing in a video. Peers that don't
	// know the param (headless CDP) ignore it.
	params, _ := json.Marshal(map[string]any{
		"format":  r.spec.Format,
		"quality": r.spec.Quality,
		"settle":  false,
	})
	_ = r.pub.Send(msg.T("pane", r.paneID, "browser", "request"), &msg.MsgBrowserActionRequest{
		RequestID: r.inFlight,
		Action:    "screenshot",
		Params:    params,
	})
}

// handleFrame writes one captured frame to disk. Responses that are not ours
// (the agent's own browser_action calls share this subject) are ignored.
func (r *WebRecorderActor) handleFrame(m *msg.MsgBrowserActionResponse) {
	if r.done || !strings.HasPrefix(m.RequestID, r.reqPrefix) {
		return
	}
	// Every capture WE issued counts, whether or not we are still waiting on
	// it. A slow screenshot is not a lost one: it is a real picture of the
	// page at the moment it was requested, and dropping it because the stall
	// guard had already moved on threw away frames the browser had gone to the
	// trouble of producing.
	at, ours := r.sentAt[m.RequestID]
	if !ours {
		return // already recorded, or pruned long ago
	}
	delete(r.sentAt, m.RequestID)
	if m.RequestID == r.inFlight {
		r.inFlight = ""
	}
	if !m.Success || m.Screenshot == "" {
		r.failed++
		return
	}
	data, err := base64.StdEncoding.DecodeString(m.Screenshot)
	if err != nil || len(data) == 0 {
		r.failed++
		return
	}
	// A peer that predates the format/quality params returns PNG regardless of
	// what we asked for, so trust the bytes rather than the request.
	name := fmt.Sprintf("frame-%06d.%s", len(r.frames), frameExt(data, r.spec.FrameExt()))
	if err := os.WriteFile(filepath.Join(r.framesDir, name), data, 0o644); err != nil {
		r.failed++
		return
	}
	// Stamped with when the capture was REQUESTED, not when its answer landed:
	// that is when the pixels were of, and it keeps a late frame in its true
	// place in the timeline rather than bunched up against whatever followed.
	r.frames = append(r.frames, recFrame{name: name, at: at})
}

// pruneSentAt forgets captures too old to still be in flight, so a peer that
// answers nothing cannot grow the map for the length of a long run. The window
// is deliberately generous — many times the stall guard's — because the whole
// point of keeping these is to accept an answer the guard already gave up on.
func (r *WebRecorderActor) pruneSentAt() {
	if len(r.sentAt) == 0 {
		return
	}
	cutoff := time.Since(r.startedAt) - sentAtRetention(r.spec.Interval)
	for id, at := range r.sentAt {
		if at < cutoff {
			delete(r.sentAt, id)
		}
	}
}

// sentAtRetention is how long a requested capture stays claimable. Floored so
// a very short interval still leaves a usable window on a slow machine.
func sentAtRetention(interval time.Duration) time.Duration {
	if d := 30 * interval; d > 10*time.Second {
		return d
	}
	return 10 * time.Second
}

// handleStep is the unsupervised run-end detector. It reuses the supervisor's
// classification so both agree on what "the run ended" means.
func (r *WebRecorderActor) handleStep(ctx actor.Context, m *msg.MsgAgenticStep) {
	if r.done || r.supervised || m.Depth != 0 {
		return
	}
	switch classifyLoopStep(m.Kind, m.Origin) {
	case loopActionRunning:
		r.graceSeq++ // run (re)started — invalidate a pending grace timer
	case loopActionPassEnd:
		r.finish(ctx, "run finished")
	case loopActionAbort:
		r.finish(ctx, "run stopped: "+abortReason(m.Kind, m.Origin))
	case loopActionGrace:
		// A paused(max_*) run usually restarts within moments via
		// auto-continue or the finalizer; only a silent grace window means
		// the run is really over.
		r.graceSeq++
		seq, self, system := r.graceSeq, ctx.Self(), ctx.ActorSystem()
		time.AfterFunc(loopPassGrace, func() {
			system.Root.Send(self, &recGraceElapsed{seq: seq})
		})
	}
}

// finish stops capture, encodes, reports into the pane, and stops the actor.
func (r *WebRecorderActor) finish(ctx actor.Context, reason string) {
	if r.done {
		return
	}
	r.done = true
	if r.quit != nil {
		close(r.quit)
		r.quit = nil
	}

	elapsed := time.Since(r.startedAt).Round(time.Second)
	if len(r.frames) == 0 {
		_ = r.pub.SendPaneRyshOutput(r.paneID,
			fmt.Sprintf("[web] recording: no frames captured (%s) — nothing to encode\n", reason))
		_ = os.RemoveAll(r.framesDir)
		ctx.Stop(ctx.Self())
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n[web] recording: %s — %d frames over %s", reason, len(r.frames), elapsed)
	if r.dropped > 0 || r.failed > 0 || r.timedOut > 0 {
		fmt.Fprintf(&sb, " (%d ticks dropped, %d failed, %d timed out)", r.dropped, r.failed, r.timedOut)
	}
	sb.WriteString("\n")

	if err := r.encode(); err != nil {
		// Never fail the automation over the recording. Keep the frames and
		// hand the user the exact command to finish the job themselves.
		fmt.Fprintf(&sb, "[web] recording: could not encode (%v)\n", err)
		fmt.Fprintf(&sb, "[web] recording: frames kept at %s\n", r.framesDir)
		fmt.Fprintf(&sb, "[web] recording: encode with: ffmpeg %s\n",
			strings.Join(ffmpegArgs(filepath.Join(r.framesDir, concatFileName), r.outPath), " "))
	} else {
		fmt.Fprintf(&sb, "[web] recording: %s\n", r.outPath)
		_ = os.RemoveAll(r.framesDir)
	}
	_ = r.pub.SendPaneRyshOutput(r.paneID, sb.String())
	slog.Info("web-record: finished", "pane", r.paneID, "frames", len(r.frames),
		"dropped", r.dropped, "failed", r.failed, "timed_out", r.timedOut, "out", r.outPath)
	ctx.Stop(ctx.Self())
}

// encode turns the frames into the output video via ffmpeg's concat demuxer.
func (r *WebRecorderActor) encode() error {
	// Frames are appended in ARRIVAL order but timestamped by REQUEST time, and
	// once a capture the stall guard gave up on can still be kept, a slow
	// answer can land after a later, faster one. Order by capture time so the
	// video plays in the order the pixels actually happened.
	sort.Slice(r.frames, func(i, j int) bool { return r.frames[i].at < r.frames[j].at })

	concatPath := filepath.Join(r.framesDir, concatFileName)
	if err := os.WriteFile(concatPath, []byte(buildConcatFile(r.frames, r.spec.Interval)), 0o644); err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH")
	}
	if err := os.MkdirAll(filepath.Dir(r.outPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	// Encode to a hidden sibling and rename on success: ffmpeg writes the file
	// incrementally (moov atom last), so anything watching outPath — including
	// the integration tests' size>0 poll — must only ever see a finished,
	// probe-able video. Same directory keeps the rename atomic; same extension
	// keeps ffmpeg's extension-based muxer selection.
	tmpPath := filepath.Join(filepath.Dir(r.outPath), ".part-"+filepath.Base(r.outPath))
	cmd := exec.Command(bin, ffmpegArgs(concatPath, tmpPath)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ffmpeg: %w: %s", err, lastLines(string(out), 3))
	}
	if err := os.Rename(tmpPath, r.outPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize video: %w", err)
	}
	return nil
}

// concatFileName is the ffmpeg concat demuxer list written into the frames dir.
const concatFileName = "frames.txt"

// buildConcatFile renders the ffmpeg concat demuxer list. Each frame's
// duration is the gap to the NEXT frame's capture time, so dropped ticks
// stretch the frame that was on screen instead of speeding the video up —
// the result plays back at true wall-clock rate. The final frame is listed
// twice: the concat demuxer ignores the duration of the last entry, and the
// repeat is the standard way to make it stick.
func buildConcatFile(frames []recFrame, interval time.Duration) string {
	var sb strings.Builder
	sb.WriteString("ffconcat version 1.0\n")
	for i, f := range frames {
		d := interval
		if i+1 < len(frames) {
			if gap := frames[i+1].at - f.at; gap > 0 {
				d = gap
			}
		}
		fmt.Fprintf(&sb, "file '%s'\nduration %.3f\n", f.name, d.Seconds())
	}
	if n := len(frames); n > 0 {
		fmt.Fprintf(&sb, "file '%s'\n", frames[n-1].name)
	}
	return sb.String()
}

// ffmpegArgs builds the encode command. The scale filter rounds odd dimensions
// down to even numbers because yuv420p (needed for broad player support)
// cannot represent odd sizes, and browser viewports are frequently odd.
// -vsync vfr keeps the concat list's per-frame durations as real output
// timestamps: without it, ffmpeg 4.x resamples concat input to constant
// 25fps, duplicating every captured frame several times over (newer ffmpeg
// preserves timestamps by default — this pins the one behavior everywhere).
func ffmpegArgs(concatPath, outPath string) []string {
	return []string{
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", concatPath,
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-vsync", "vfr",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		outPath,
	}
}

// frameExt identifies the image format from its magic bytes, falling back to
// the requested extension when the bytes are unrecognised.
func frameExt(data []byte, fallback string) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg"
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "webp"
	}
	return fallback
}

// lastLines trims noisy tool output down to its final n lines.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
