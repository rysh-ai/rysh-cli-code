package actors

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// animatedPage is a data: URL whose content changes several times a second, so
// consecutive frames genuinely differ — a recorder that captured the same
// image every tick, or captured nothing after the first frame, would still
// produce a plausible-looking file, and this makes that detectable.
const animatedPage = `data:text/html,` +
	`<html><body style="margin:0;background:%23111">` +
	`<div id="t" style="font:96px monospace;color:%230f0;padding:40px">0</div>` +
	`<script>let n=0;setInterval(()=>{n++;` +
	`document.getElementById("t").textContent=n;` +
	`document.body.style.background="hsl("+(n*40%25360)+",70%25,25%25)";},100)</script>` +
	`</body></html>`

// TestIntegration_WebRecorderProducesVideo is the live end-to-end test of
// `##auto web run --record` minus the LLM: a real headless Chromium, a real
// embedded NATS broker, the real HeadlessBrowserActor serving
// pane.{id}.browser.request, the real WebRecorderActor capturing through it,
// and a real ffmpeg encode. It asserts an actually-playable video comes out
// with roughly the frame count the interval implies.
//
// Skipped when Chromium or ffmpeg is unavailable (CI without either).
func TestIntegration_WebRecorderProducesVideo(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	// CI containers ship a Chrome binary that cannot actually start: the
	// launcher passes --headless=new with no --no-sandbox /
	// --disable-dev-shm-usage, so FindChromium succeeds, the launch never
	// answers, and the test would burn its full probe window before failing.
	// .github/workflows/ci.yml already excludes internal/cdp for exactly this
	// reason; this package cannot be excluded wholesale, so the browser-launch
	// test opts out here. Announced, not silent — a green CI run does NOT mean
	// the headless browser path was covered.
	if os.Getenv("CI") != "" {
		t.Skip("CI: browser-launch integration not covered (container Chrome cannot start without --no-sandbox)")
	}
	if cdp.FindChromium() == "" {
		t.Skip("no Chromium/Chrome binary available")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	// --- embedded broker ---
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats not ready")
	}
	nc, err := nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	system := actor.NewActorSystem()
	paneID := "recpane"

	// --- the browser peer that serves pane.{id}.browser.request ---
	// The Chromium profile deliberately does NOT use t.TempDir(): Chromium
	// keeps writing to its user-data-dir until the process actually exits, and
	// t.TempDir()'s cleanup would race that and fail the test. Own the
	// directory here so it can be removed after the browser is confirmed down.
	profileDir, err := os.MkdirTemp("", "rysh-rec-e2e-*")
	if err != nil {
		t.Fatalf("temp profile dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(profileDir) }() // LIFO: runs after the stop below

	hb := NewHeadlessBrowserActor(paneID, "e2e", animatedPage, profileDir, pub, nc)
	hbPID := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return hb }))
	defer func() {
		_ = system.Root.StopFuture(hbPID).Wait() // Stop() alone is async
		time.Sleep(500 * time.Millisecond)       // let Chromium flush and exit
	}()

	// Handshake rather than sleep: drive one screenshot ourselves and wait for
	// the reply, which proves Chromium launched and CDP is answering.
	waitBrowserReady(t, nc, pub, paneID)

	// --- the recorder under test ---
	//
	// The recording is driven by frames actually CAPTURED, not by a wall-clock
	// sleep. Recording for a fixed 3s and asserting "~15 frames arrived" makes
	// the test a benchmark of the machine: on a loaded box Chromium managed 2
	// screenshots in that window, sometimes 0, and the assertions below fired
	// on a recorder that was working correctly. Waiting for real captures
	// costs nothing when the machine is idle and simply takes longer when it
	// is not.
	const interval = 200 * time.Millisecond
	const wantCaptures = 6          // enough to encode, and to see the page change
	const minSpan = 1 * time.Second // ...spread over enough real time to animate
	const captureCeiling = 90 * time.Second
	outPath := filepath.Join(t.TempDir(), "run.mp4")
	spec := webauto.ResolveRecord(nil, nil, webauto.RecordFlags{On: true, Interval: interval})

	// Watch the recorder's own capture traffic: every browser.response that is
	// not our readiness probe is one frame it got back. This is the progress
	// signal the test waits on, and it also gives the REAL capture span to
	// check the video's duration against — the intended 3s was never the right
	// yardstick once captures could be dropped.
	captures := newCaptureCounter(t, nc, paneID)
	// Subscribe to the recorder's completion report BEFORE it can be sent.
	// It carries the encode error when there is one, which the old
	// poll-for-a-file loop could only report as a blank 60-second timeout.
	report := newRecorderReport(t, nc, paneID)

	rec := NewWebRecorderActor(paneID, "e2e-recipe", spec, outPath, true /*supervised*/, pub, nc)
	recPID := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return rec }))

	startedAt := time.Now()
	deadline := time.Now().Add(captureCeiling)
	for {
		n, span := captures.state()
		if n >= wantCaptures && time.Since(startedAt) >= minSpan {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d captures in %s (span %s) — the browser is not answering, "+
				"not merely slow", n, captureCeiling, span.Round(time.Millisecond))
		}
		time.Sleep(50 * time.Millisecond)
	}
	system.Root.Send(recPID, &recStop{reason: "test done"})

	nCaptures, span := captures.state()

	// --- wait for the recorder to say it finished, and why ---
	msgText := report.wait(t, 90*time.Second)
	if strings.Contains(msgText, "could not encode") || strings.Contains(msgText, "no frames captured") {
		t.Fatalf("recorder did not produce a video: %s", strings.TrimSpace(msgText))
	}
	fi, err := os.Stat(outPath)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("recorder reported success but no video at %s (err=%v): %s",
			outPath, err, strings.TrimSpace(msgText))
	}
	size := fi.Size()

	// --- assert it is a real, playable video ---
	nbFrames, duration := probeVideo(t, outPath)
	t.Logf("video: %s (%d bytes, %d frames, %.2fs) from %d captures over %s",
		outPath, size, nbFrames, duration, nCaptures, span.Round(time.Millisecond))

	// The encode must neither invent frames nor lose them, measured against
	// what was actually captured rather than against the clock.
	//
	// The expected count is captures+1: buildConcatFile lists the final frame
	// twice on purpose, because the concat demuxer ignores the last entry's
	// duration and the repeat is what makes it stick. Anything above that is
	// the bug -vsync vfr exists to prevent — ffmpeg resampling the concat list
	// to a constant 25fps, duplicating every frame several times over — which
	// would show up as tens of frames, not one. Anything below captures means
	// frames were captured and dropped on the floor (one frame of slack: a
	// capture can land after the count is read but before the recorder stops).
	if nbFrames > nCaptures+1 {
		t.Errorf("got %d frames from %d captures — the encode is duplicating frames "+
			"(expected %d: every capture plus the deliberate final repeat)",
			nbFrames, nCaptures, nCaptures+1)
	}
	if nbFrames < nCaptures {
		t.Errorf("got %d frames from %d captures — the encode is losing frames", nbFrames, nCaptures)
	}
	// Per-frame durations must reconstruct real elapsed time, not fps*frames.
	// Measured against the captures' true span, so a loaded machine that
	// spread them over 10s is judged against 10s rather than a fixed guess.
	if want := span.Seconds() * 0.5; duration < want {
		t.Errorf("video is %.2fs, want >=%.2fs — the %s capture span is not reflected "+
			"in the frame timings", duration, want, span.Round(time.Millisecond))
	}

	// The video must show the run CHANGING. A recorder that captured one
	// static image (or the same cached frame every tick) would still produce a
	// well-formed file with a plausible frame count, so compare real decoded
	// frames rather than trusting the count.
	assertFramesDiffer(t, outPath)

	// The frames directory is scratch: a successful encode must clean it up.
	framesDir := strings.TrimSuffix(outPath, ".mp4") + ".frames"
	if _, err := os.Stat(framesDir); !os.IsNotExist(err) {
		t.Errorf("frames dir %s should be removed after a successful encode", framesDir)
	}
}

// captureCounter tallies the frames the recorder actually got back, by
// watching the same browser.response subject the recorder consumes.
//
// It exists so the test can wait for real progress instead of sleeping for a
// fixed wall-clock window and hoping. It also records when the first and last
// capture landed, which is the only honest yardstick for the encoded video's
// duration: the intended recording length stopped being meaningful the moment
// captures could be dropped.
type captureCounter struct {
	mu    sync.Mutex
	n     int
	first time.Time
	last  time.Time
}

// newCaptureCounter subscribes and starts counting. The subscription is
// unsubscribed when the test ends.
func newCaptureCounter(t *testing.T, nc *nats.Conn, paneID string) *captureCounter {
	t.Helper()
	c := &captureCounter{}
	sub, err := nc.Subscribe(msg.T("pane", paneID, "browser", "response"), func(m *nats.Msg) {
		var env struct {
			TypeTag string          `json:"t"`
			Payload json.RawMessage `json:"p"`
		}
		if json.Unmarshal(m.Data, &env) != nil || env.TypeTag != "MsgBrowserActionResponse" {
			return
		}
		var resp msg.MsgBrowserActionResponse
		if json.Unmarshal(env.Payload, &resp) != nil {
			return
		}
		// Skip the readiness probe (waitBrowserReady) and anything that
		// failed — only frames the recorder can actually encode count.
		if resp.RequestID == "probe-1" || !resp.Success {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.n++
		now := time.Now()
		if c.first.IsZero() {
			c.first = now
		}
		c.last = now
	})
	if err != nil {
		t.Fatalf("subscribe browser.response: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return c
}

// state returns how many frames have been captured and the span they cover.
func (c *captureCounter) state() (n int, span time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n > 1 {
		span = c.last.Sub(c.first)
	}
	return c.n, span
}

// recorderReport captures the recorder's own completion line from the pane's
// rysh output — "[web] recording: <path>", or the reason it produced nothing.
//
// The test used to poll for the output file for 60 seconds and, on failure,
// could say only "no video produced after 60s". The recorder knew exactly what
// went wrong (an ffmpeg error, or zero frames to encode) and was saying so
// into the pane the whole time; listening for that turns a blank minute-long
// timeout into the actual reason, immediately.
type recorderReport struct {
	ch chan string
}

func newRecorderReport(t *testing.T, nc *nats.Conn, paneID string) *recorderReport {
	t.Helper()
	r := &recorderReport{ch: make(chan string, 4)}
	sub, err := nc.Subscribe(msg.T("pane", paneID, "output", "rysh"), func(m *nats.Msg) {
		var env struct {
			TypeTag string          `json:"t"`
			Payload json.RawMessage `json:"p"`
		}
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		var appended msg.MsgConversationAppend
		if json.Unmarshal(env.Payload, &appended) != nil || appended.Message == nil {
			return
		}
		// The recorder emits progress lines too; only the terminal report
		// names the output file or says why there is none.
		text := appended.Message.Content
		if strings.Contains(text, "recording:") &&
			(strings.Contains(text, ".mp4") ||
				strings.Contains(text, "could not encode") ||
				strings.Contains(text, "no frames captured")) {
			select {
			case r.ch <- text:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("subscribe pane rysh output: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return r
}

// wait blocks for the recorder's completion report, failing the test if it
// never arrives.
func (r *recorderReport) wait(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case text := <-r.ch:
		return text
	case <-time.After(timeout):
		t.Fatalf("recorder never reported finishing within %s", timeout)
		return ""
	}
}

// assertFramesDiffer decodes the video back to PNGs and fails if every frame is
// byte-identical — i.e. the recording captured a frozen image rather than the
// page as it changed.
func assertFramesDiffer(t *testing.T, videoPath string) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y",
		"-i", videoPath, filepath.Join(dir, "out-%03d.png")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg extract frames: %v: %s", err, out)
	}
	shots, err := filepath.Glob(filepath.Join(dir, "out-*.png"))
	if err != nil || len(shots) < 2 {
		t.Fatalf("expected >=2 decoded frames, got %d (err=%v)", len(shots), err)
	}
	first, err := os.ReadFile(shots[0])
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	for _, p := range shots[1:] {
		other, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if !bytes.Equal(first, other) {
			return // at least one frame differs — the page was really captured
		}
	}
	t.Errorf("all %d frames are identical — the recording captured a frozen image", len(shots))
}

// waitBrowserReady round-trips one screenshot through the browser actor,
// failing the test if Chromium never answers.
func waitBrowserReady(t *testing.T, nc *nats.Conn, pub *msg.NATSPublisher, paneID string) {
	t.Helper()
	ready := make(chan struct{})
	sub, err := nc.Subscribe(msg.T("pane", paneID, "browser", "response"), func(m *nats.Msg) {
		var env struct {
			TypeTag string          `json:"t"`
			Payload json.RawMessage `json:"p"`
		}
		if json.Unmarshal(m.Data, &env) != nil || env.TypeTag != "MsgBrowserActionResponse" {
			return
		}
		var resp msg.MsgBrowserActionResponse
		if json.Unmarshal(env.Payload, &resp) == nil && resp.RequestID == "probe-1" {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("subscribe browser.response: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Chromium's cold start can take a few seconds; retry the probe until it
	// answers rather than guessing a single sleep duration.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_ = pub.Send(msg.T("pane", paneID, "browser", "request"), &msg.MsgBrowserActionRequest{
			RequestID: "probe-1",
			Action:    "screenshot",
			Params:    json.RawMessage(`{}`),
		})
		select {
		case <-ready:
			return
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatal("headless browser never answered a screenshot request")
}

// probeVideo returns the frame count and duration ffprobe reports for a file,
// failing the test if it is not a decodable video.
func probeVideo(t *testing.T, path string) (frames int, duration float64) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "v:0",
		"-count_frames",
		"-show_entries", "stream=nb_read_frames",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		t.Fatalf("ffprobe gave no video stream for %s: %q", path, string(out))
	}
	if frames, err = strconv.Atoi(fields[0]); err != nil {
		t.Fatalf("frame count %q: %v", fields[0], err)
	}
	if duration, err = strconv.ParseFloat(fields[1], 64); err != nil {
		t.Fatalf("duration %q: %v", fields[1], err)
	}
	if frames == 0 {
		t.Fatalf("%s decodes to zero frames", path)
	}
	return frames, duration
}
