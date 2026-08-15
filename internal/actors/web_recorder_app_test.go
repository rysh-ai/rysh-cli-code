// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestIntegration_WebRecorderThroughAppBridge proves `##auto web run --record`
// works when the browser is the DESKTOP APP's embedded WebContentsView rather
// than the CLI-owned headless Chromium.
//
// The recorder is browser-agnostic by construction — it speaks
// pane.{id}.browser.request/response and does not care which peer answers — so
// what actually needs proving is the daemon's WebSocket bridge to the app:
//
//	WebRecorderActor
//	  → pane.{id}.browser.request        (NATS)
//	  → Server.subscribeBrowserRequests  → hub.sendAll → WebSocket "browser_action"
//	  → [the Electron renderer]          → IPC → webPaneExecutor.screenshot()
//	  → WebSocket "browser_result"       → Server.handleCommand
//	  → pane.{id}.browser.response       (NATS)
//	  → WebRecorderActor writes the frame
//
// Everything above runs for real here except the Electron renderer itself,
// which is stood in for by a WebSocket client returning genuine JPEG frames —
// the same encoding and roughly the same size the app's captureScreenshotJPEG
// produces (JPEG q40, capped at 1280 wide). Electron's own capturePage is the
// only link not covered, and it is ordinary app code.
func TestIntegration_WebRecorderThroughAppBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	// --- in-process broker shared by the recorder and the web server ---
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, DontListen: true,
	})
	if err != nil {
		t.Skipf("embedded NATS unavailable: %v", err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats not ready")
	}
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	// --- the real daemon web server (the bridge under test) ---
	port := freePort(t)
	srv := web.NewServer(port, "rec-app-test", pub, nc, pub.Codecs())
	go func() { _ = srv.Start() }()
	defer func() { _ = srv.Stop() }()
	waitHealthy(t, port)

	// --- stand-in for the Electron renderer ---
	paneID := "apppane"
	fake := newFakeRenderer(t, port, paneID)
	defer fake.close()

	// --- the recorder, unchanged from the headless path ---
	const interval = 200 * time.Millisecond
	const recordFor = 3 * time.Second
	outPath := filepath.Join(t.TempDir(), "app-run.mp4")
	spec := webauto.ResolveRecord(nil, nil, webauto.RecordFlags{On: true, Interval: interval})

	system := actor.NewActorSystem()
	rec := NewWebRecorderActor(paneID, "app-recipe", spec, outPath, true, pub, nc)
	recPID := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return rec }))

	time.Sleep(recordFor)
	system.Root.Send(recPID, &recStop{reason: "test done"})

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
		t.Fatalf("no video produced at %s — the app bridge did not deliver frames", outPath)
	}

	served := fake.count()
	nbFrames, duration := probeVideo(t, outPath)
	t.Logf("app-bridge video: %d bytes, %d frames, %.2fs (renderer served %d screenshots)",
		size, nbFrames, duration, served)

	if served == 0 {
		t.Fatal("the renderer was never asked for a screenshot — the bridge did not forward the request")
	}
	// Capture hygiene the app depends on: every recorder frame must opt out of
	// the pre-capture settle, and must carry the recording quality so the app
	// isn't stuck on its agent-tuned default.
	fake.mu.Lock()
	misses, quality := fake.settleMisses, fake.lastQuality
	fake.mu.Unlock()
	if misses > 0 {
		t.Errorf("%d/%d screenshot requests lacked settle:false — the app would inject "+
			"requestAnimationFrame+80ms into the automated page on every frame", misses, served)
	}
	if quality != webauto.DefaultRecordQuality {
		t.Errorf("screenshot quality = %d, want %d (the resolved record spec)", quality, webauto.DefaultRecordQuality)
	}
	// Every screenshot the stand-in renderer served must reach the recorder, so
	// the encoded frame count should track it closely. A shortfall means frames
	// were lost in the bridge (payload limits, correlation, hub buffering).
	if nbFrames < served-2 {
		t.Errorf("renderer served %d screenshots but the video has %d frames — frames lost in the bridge",
			served, nbFrames)
	}
	if duration < recordFor.Seconds()*0.5 {
		t.Errorf("video is %.2fs, want roughly %.2fs", duration, recordFor.Seconds())
	}
	assertFramesDiffer(t, outPath)
}

// fakeRenderer stands in for the Electron renderer's useWebSocket handler: it
// answers "browser_action" screenshot requests with a real JPEG, exactly as
// webPaneExecutor.screenshot() → captureScreenshotJPEG does.
type fakeRenderer struct {
	conn *websocket.Conn
	mu   sync.Mutex
	// served counts screenshot requests answered; settleMisses counts those
	// that did NOT carry settle:false; lastQuality is the last quality asked
	// for (0 when absent).
	served       int
	settleMisses int
	lastQuality  int
	done         chan struct{}
}

func newFakeRenderer(t *testing.T, port int, paneID string) *fakeRenderer {
	t.Helper()
	url := fmt.Sprintf("ws://127.0.0.1:%d/ws", port)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	f := &fakeRenderer{conn: conn, done: make(chan struct{})}

	go func() {
		defer close(f.done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var envelope struct {
				Type string `json:"type"`
				Data struct {
					PaneID  string `json:"pane_id"`
					Request struct {
						RequestID string          `json:"request_id"`
						Action    string          `json:"action"`
						Params    json.RawMessage `json:"params"`
					} `json:"request"`
				} `json:"data"`
			}
			if json.Unmarshal(data, &envelope) != nil || envelope.Type != "browser_action" {
				continue
			}
			req := envelope.Data.Request
			if req.Action != "screenshot" || envelope.Data.PaneID != paneID {
				continue
			}

			// The recorder must ask the app to skip its pre-capture settle;
			// without this the app injects requestAnimationFrame+80ms into the
			// automated page on every frame for the whole run.
			var got struct {
				Settle  *bool  `json:"settle"`
				Format  string `json:"format"`
				Quality int    `json:"quality"`
			}
			_ = json.Unmarshal(req.Params, &got)

			f.mu.Lock()
			n := f.served
			f.served++
			if got.Settle == nil || *got.Settle {
				f.settleMisses++
			}
			f.lastQuality = got.Quality
			f.mu.Unlock()

			// Mirrors the renderer's sendCommand('browser_result', {...}):
			// the hub only dispatches messages shaped
			// {type:"command", data:{action, params}}.
			reply, _ := json.Marshal(map[string]any{
				"type": "command",
				"data": map[string]any{
					"action": "browser_result",
					"params": map[string]any{
						"pane_id":    envelope.Data.PaneID,
						"request_id": req.RequestID,
						"success":    true,
						"result":     map[string]any{"format": "jpeg"},
						"error":      "",
						"screenshot": jpegFrame(n),
					},
				},
			})
			f.mu.Lock()
			err = conn.WriteMessage(websocket.TextMessage, reply)
			f.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return f
}

func (f *fakeRenderer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served
}

func (f *fakeRenderer) close() {
	_ = f.conn.Close()
	<-f.done
}

// jpegFrame renders a distinct 1280x800 JPEG for frame n — the app caps
// captures at 1280 wide and encodes JPEG q40, so this matches the real payload
// size closely enough to exercise the hub's write buffer limits.
func jpegFrame(n int) string {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 800))
	bg := color.RGBA{R: uint8(30 + n*7%200), G: uint8(60 + n*13%180), B: uint8(90 + n*3%150), A: 255}
	for y := 0; y < 800; y++ {
		for x := 0; x < 1280; x++ {
			img.Set(x, y, bg)
		}
	}
	// A moving block guarantees the frames differ structurally, not just in
	// their flat background colour.
	for y := 100; y < 300; y++ {
		for x := 50 + (n*37)%900; x < 250+(n*37)%900; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 40})
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// freePort reserves an ephemeral port and releases it for the server to bind.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// waitHealthy blocks until the web server answers /health.
func waitHealthy(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("web server on :%d never became healthy", port)
}
