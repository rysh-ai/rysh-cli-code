package actors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	const interval = 200 * time.Millisecond
	const recordFor = 3 * time.Second
	outPath := filepath.Join(t.TempDir(), "run.mp4")
	spec := webauto.ResolveRecord(nil, nil, webauto.RecordFlags{On: true, Interval: interval})

	rec := NewWebRecorderActor(paneID, "e2e-recipe", spec, outPath, true /*supervised*/, pub, nc)
	recPID := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return rec }))

	time.Sleep(recordFor)
	system.Root.Send(recPID, &recStop{reason: "test done"})

	// --- the encode is asynchronous; wait for the file to appear and settle ---
	var size int64
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(outPath); err == nil && fi.Size() > 0 {
			size = fi.Size()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if size == 0 {
		t.Fatalf("no video produced at %s after 60s", outPath)
	}

	// --- assert it is a real, playable video ---
	nbFrames, duration := probeVideo(t, outPath)
	t.Logf("video: %s (%d bytes, %d frames, %.2fs)", outPath, size, nbFrames, duration)

	// The recorder is drop-don't-queue, so the frame count is a range, not an
	// equality: it can never EXCEED the tick count, and a large shortfall means
	// captures were failing rather than merely being dropped.
	maxFrames := int(recordFor/interval) + 2
	minFrames := maxFrames / 3
	if nbFrames > maxFrames {
		t.Errorf("got %d frames, more than the %d ticks the interval allows", nbFrames, maxFrames)
	}
	if nbFrames < minFrames {
		t.Errorf("got %d frames, want >=%d — captures are failing, not just dropping", nbFrames, minFrames)
	}
	// Per-frame durations should reconstruct real elapsed time, not fps*frames.
	if duration < recordFor.Seconds()*0.5 {
		t.Errorf("video is %.2fs, want roughly %.2fs — wall-clock timing is wrong", duration, recordFor.Seconds())
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
		t.Fatal(fmt.Sprintf("%s decodes to zero frames", path))
	}
	return frames, duration
}
