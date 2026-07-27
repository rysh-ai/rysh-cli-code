package replay

import (
	"path/filepath"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The durable half's whole reason to exist is that the in-memory Capture dies
// with the daemon (design 006 §3.1). So the test that matters is: publish
// output, drop the capture (and the broker), bring both back on the same
// store dir, and export still produces the recording. A single-process test
// on the in-memory view would stay green through exactly the gap being
// closed.

// startJetStreamNATS starts an in-process, JetStream-enabled NATS server
// backed by storeDir (so a second server on the same dir recovers its
// streams) and returns a client plus the server (for an explicit restart).
func startJetStreamNATS(t *testing.T, storeDir string) (*nats.Conn, *natsserver.Server) {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  storeDir,
	})
	if err != nil {
		t.Fatalf("new jetstream nats: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("jetstream nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect jetstream nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc, ns
}

// publishAppend publishes one MsgConversationAppend envelope on the pane's
// merged output subject — the exact wire shape PaneActors produce.
func publishAppend(t *testing.T, pub *msg.NATSPublisher, paneID, content string, tsMs int64) {
	t.Helper()
	err := pub.Send(msg.T("pane", paneID, "output"), &msg.MsgConversationAppend{
		Message: &msg.ConversationMessage{Content: content, TimestampMs: tsMs},
	})
	if err != nil {
		t.Fatalf("publish append: %v", err)
	}
}

// publishResized publishes one MsgPaneResized envelope on the pane's resized
// subject — the exact wire shape PaneActor produces after a resize.
func publishResized(t *testing.T, pub *msg.NATSPublisher, paneID string, cols, rows int) {
	t.Helper()
	err := pub.Send(msg.T("pane", paneID, "resized"),
		&msg.MsgPaneResized{PaneID: paneID, Cols: cols, Rows: rows})
	if err != nil {
		t.Fatalf("publish resized: %v", err)
	}
}

// waitStreamMsgs polls until the stream holds want messages.
func waitStreamMsgs(t *testing.T, nc *nats.Conn, stream string, want uint64) {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := js.StreamInfo(stream); err == nil && info.State.Msgs >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stream %s never reached %d message(s)", stream, want)
}

// TestDurableCaptureStoresAndExportsFromStream: publishes on the pane output
// subject land in the stream with no capture subscription involved, and
// export rebuilds the same events from the stream when the in-memory buffer
// is empty.
func TestDurableCaptureStoresAndExportsFromStream(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	const session = "durable-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c := NewCapture(nc, codecs, session)
	if err := c.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}

	pub := msg.NewNATSPublisher(nc, codecs)
	publishAppend(t, pub, "p1", "$ ls\n", 1000)
	publishAppend(t, pub, "p1", "file.go\n", 2500)
	publishAppend(t, pub, "p2", "other pane\n", 1500)
	waitStreamMsgs(t, nc, StreamName(session), 3)

	// Exactly one stored copy per publish — the stream captures the subject
	// directly, no double-publishing.
	if on, msgs := c.DurableInfo(); !on || msgs != 3 {
		t.Fatalf("DurableInfo = (%v, %d), want (true, 3)", on, msgs)
	}

	// The in-memory buffer is empty (Start was never called), so this export
	// must come from the stream.
	if c.Count("") != 0 {
		t.Fatalf("in-memory count = %d, want 0", c.Count(""))
	}
	path := filepath.Join(t.TempDir(), "p1.cast")
	n, err := c.ExportToFile("p1", path, "t")
	if err != nil || n != 2 {
		t.Fatalf("export: n=%d err=%v", n, err)
	}
	evs := castEvents(t, path)
	if len(evs) != 2 || evs[0][2] != "$ ls\n" || evs[1][2] != "file.go\n" {
		t.Fatalf("events = %v, want p1's two outputs in order", evs)
	}
	if evs[1][0].(float64) != 1.5 { // (2500-1000)/1000
		t.Fatalf("second event time = %v, want 1.5", evs[1][0])
	}

	// Merged export sees all panes, timestamp-ordered.
	allPath := filepath.Join(t.TempDir(), "all.cast")
	if n, err := c.ExportToFile("", allPath, "t"); err != nil || n != 3 {
		t.Fatalf("merged export: n=%d err=%v", n, err)
	}
	all := castEvents(t, allPath)
	if all[1][2] != "other pane\n" {
		t.Fatalf("merged order = %v, want p2's 1500ms event second", all)
	}
}

// TestDurableStreamCapturesResizeEvents: the durable stream also captures the
// pane resize subject, so the dimension timeline survives a restart instead of
// living only in the RAM ring (the gap that made post-restart exports fall
// back to 80x24).
func TestDurableStreamCapturesResizeEvents(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	const session = "resize-durable-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c := NewCapture(nc, codecs, session)
	if err := c.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	pub := msg.NewNATSPublisher(nc, codecs)
	publishResized(t, pub, "p1", 100, 30)
	publishAppend(t, pub, "p1", "$ ls\n", 1000)
	publishResized(t, pub, "p2", 120, 40)
	waitStreamMsgs(t, nc, StreamName(session), 3)

	// The resize timeline is rebuildable from the stream, per pane.
	rz := c.streamResizes("p1")
	if len(rz) != 1 || rz[0].d != (dim{cols: 100, rows: 30}) {
		t.Fatalf("streamResizes(p1) = %+v, want one 100x30 entry", rz)
	}
	if all := c.streamResizes(""); len(all) != 2 {
		t.Fatalf("streamResizes(\"\") = %+v, want both panes' resizes", all)
	}

	// The output walk is not confused by resize messages on the stream.
	evs := c.streamEvents("p1")
	if len(evs) != 1 || evs[0].text != "$ ls\n" {
		t.Fatalf("streamEvents(p1) = %+v, want only the output append", evs)
	}
}

// TestDurableExportSurvivesRestart: recording outlives both the Capture and
// the broker process.
func TestDurableExportSurvivesRestart(t *testing.T) {
	storeDir := t.TempDir()
	nc1, ns1 := startJetStreamNATS(t, storeDir)
	const session = "restart-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c1 := NewCapture(nc1, codecs, session)
	if err := c1.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	pub := msg.NewNATSPublisher(nc1, codecs)
	publishAppend(t, pub, "p1", "before restart\n", 1000)
	publishAppend(t, pub, "p1", "still here\n", 2000)
	waitStreamMsgs(t, nc1, StreamName(session), 2)

	nc1.Close()
	ns1.Shutdown()
	ns1.WaitForShutdown()

	nc2, _ := startJetStreamNATS(t, storeDir)
	c2 := NewCapture(nc2, codecs, session)
	if err := c2.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable after restart: %v", err)
	}
	if on, msgs := c2.DurableInfo(); !on || msgs != 2 {
		t.Fatalf("DurableInfo after restart = (%v, %d), want (true, 2)", on, msgs)
	}
	path := filepath.Join(t.TempDir(), "r.cast")
	n, err := c2.ExportToFile("p1", path, "t")
	if err != nil || n != 2 {
		t.Fatalf("export after restart: n=%d err=%v", n, err)
	}
	evs := castEvents(t, path)
	if evs[0][2] != "before restart\n" || evs[1][2] != "still here\n" {
		t.Fatalf("events after restart = %v", evs)
	}
}

// TestDurableExportSurvivesRestartWithDims: after a restart wipes the RAM
// ring, export rebuilds not just the output but also the dimension timeline
// from the durable stream — header from the size in effect at the first
// output, mid-recording changes as asciicast "r" events. Before the stream
// captured .resized, this export fell back to a hardcoded 80x24.
func TestDurableExportSurvivesRestartWithDims(t *testing.T) {
	storeDir := t.TempDir()
	nc1, ns1 := startJetStreamNATS(t, storeDir)
	const session = "restart-dims-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c1 := NewCapture(nc1, codecs, session)
	if err := c1.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	pub := msg.NewNATSPublisher(nc1, codecs)

	// Resize timestamps come from JetStream receipt time (the same wall clock
	// production output timestamps use), so the appends here must carry real
	// wall-clock timestamps too.
	publishResized(t, pub, "p1", 100, 30) // before first output → header
	waitStreamMsgs(t, nc1, StreamName(session), 1)
	base := time.Now().UnixMilli()
	publishAppend(t, pub, "p1", "before\n", base)
	waitStreamMsgs(t, nc1, StreamName(session), 2)
	// The mid-recording resize must be receipt-stamped strictly after the
	// first output's timestamp; without a gap both can land in the same
	// millisecond and the header logic would (correctly) treat the resize as
	// pre-first-output.
	time.Sleep(15 * time.Millisecond)
	publishResized(t, pub, "p1", 120, 40) // mid-recording → "r" event
	publishAppend(t, pub, "p1", "after\n", base+5000)
	waitStreamMsgs(t, nc1, StreamName(session), 4)

	nc1.Close()
	ns1.Shutdown()
	ns1.WaitForShutdown()

	nc2, _ := startJetStreamNATS(t, storeDir)
	c2 := NewCapture(nc2, codecs, session)
	if err := c2.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable after restart: %v", err)
	}
	if c2.Count("") != 0 {
		t.Fatalf("fresh capture ring not empty: %d", c2.Count(""))
	}

	path := filepath.Join(t.TempDir(), "dims.cast")
	if _, err := c2.ExportToFile("p1", path, "t"); err != nil {
		t.Fatalf("export after restart: %v", err)
	}
	if h := castHeader(t, path); h.Width != 100 || h.Height != 30 {
		t.Fatalf("header = %dx%d, want the pre-restart 100x30 (not the 80x24 fallback)", h.Width, h.Height)
	}
	var sawResize bool
	for _, e := range castEvents(t, path) {
		if e[1] == "r" && e[2] == "120x40" {
			sawResize = true
		}
	}
	if !sawResize {
		t.Fatal("mid-recording 120x40 resize missing from post-restart export")
	}

	// Merged export (no r-events by design) still gets a real header size:
	// the latest recorded size across the session, not 80x24.
	allPath := filepath.Join(t.TempDir(), "all.cast")
	if _, err := c2.ExportToFile("", allPath, "t"); err != nil {
		t.Fatalf("merged export after restart: %v", err)
	}
	if h := castHeader(t, allPath); h.Width != 120 || h.Height != 40 {
		t.Fatalf("merged header = %dx%d, want the latest recorded 120x40", h.Width, h.Height)
	}
}

// TestDurableRetentionApplied: options land on the stream, zero options mean
// the design 006 defaults (7d / 1GiB), and re-enabling refreshes the limits
// of an existing stream.
func TestDurableRetentionApplied(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	msg.SetSessionPrefix("retention-sess")
	codecs := msg.DefaultCodecRegistry()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	c := NewCapture(nc, codecs, "retention-sess")
	if err := c.EnableDurable(DurableOptions{Retention: 48 * time.Hour, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	info, err := js.StreamInfo(StreamName("retention-sess"))
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.Config.MaxAge != 48*time.Hour || info.Config.MaxBytes != 1<<20 {
		t.Fatalf("limits = (%v, %d), want (48h, 1MiB)", info.Config.MaxAge, info.Config.MaxBytes)
	}

	// Re-enable with new limits: the existing stream is updated, not lost.
	c2 := NewCapture(nc, codecs, "retention-sess")
	if err := c2.EnableDurable(DurableOptions{Retention: 24 * time.Hour, MaxBytes: 2 << 20}); err != nil {
		t.Fatalf("re-EnableDurable: %v", err)
	}
	info, _ = js.StreamInfo(StreamName("retention-sess"))
	if info.Config.MaxAge != 24*time.Hour || info.Config.MaxBytes != 2<<20 {
		t.Fatalf("updated limits = (%v, %d), want (24h, 2MiB)", info.Config.MaxAge, info.Config.MaxBytes)
	}

	// Zero options ⇒ package defaults. The stream subject comes from the
	// process-wide session prefix (always == the session name in production),
	// so point it at the new session first.
	msg.SetSessionPrefix("defaults-sess")
	d := NewCapture(nc, codecs, "defaults-sess")
	if err := d.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable defaults: %v", err)
	}
	info, err = js.StreamInfo(StreamName("defaults-sess"))
	if err != nil {
		t.Fatalf("StreamInfo defaults: %v", err)
	}
	if info.Config.MaxAge != DefaultRetention || info.Config.MaxBytes != DefaultMaxBytes {
		t.Fatalf("default limits = (%v, %d), want (%v, %d)",
			info.Config.MaxAge, info.Config.MaxBytes, DefaultRetention, int64(DefaultMaxBytes))
	}
}

// TestInMemoryEventsWinOverStream: live capture keeps priority — the stream
// is only consulted when the in-memory buffer is empty (the per-sequence
// fetch loop stays off the live-export path).
func TestInMemoryEventsWinOverStream(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	const session = "priority-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c := NewCapture(nc, codecs, session)
	if err := c.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	pub := msg.NewNATSPublisher(nc, codecs)
	publishAppend(t, pub, "p1", "stream copy\n", 1000)
	waitStreamMsgs(t, nc, StreamName(session), 1)

	c.byPane["p1"] = []recEvent{{ts: 1000, text: "memory copy\n"}}

	path := filepath.Join(t.TempDir(), "m.cast")
	if _, err := c.ExportToFile("p1", path, "t"); err != nil {
		t.Fatal(err)
	}
	if evs := castEvents(t, path); len(evs) != 1 || evs[0][2] != "memory copy\n" {
		t.Fatalf("events = %v, want only the in-memory copy", evs)
	}
}

// TestDeleteDurableRemovesStream: opting out removes the broker-side stream,
// so a disabled capture cannot keep storing output (design 006 opt-in).
func TestDeleteDurableRemovesStream(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	msg.SetSessionPrefix("optout-sess")
	codecs := msg.DefaultCodecRegistry()

	c := NewCapture(nc, codecs, "optout-sess")
	if err := c.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	js, _ := nc.JetStream()
	if _, err := js.StreamInfo(StreamName("optout-sess")); err != nil {
		t.Fatalf("stream should exist: %v", err)
	}

	DeleteDurable(nc, "optout-sess")
	if _, err := js.StreamInfo(StreamName("optout-sess")); err == nil {
		t.Fatal("stream still exists after DeleteDurable")
	}
}

// TestDurableInfoInactiveWithoutJetStream: a capture that never enabled the
// durable half reports it off and exports nothing from a stream.
func TestDurableInfoInactiveWithoutJetStream(t *testing.T) {
	c := NewCapture(nil, msg.DefaultCodecRegistry(), "s")
	if on, msgs := c.DurableInfo(); on || msgs != 0 {
		t.Fatalf("DurableInfo = (%v, %d), want (false, 0)", on, msgs)
	}
	if evs := c.streamEvents(""); evs != nil {
		t.Fatalf("streamEvents = %v, want nil", evs)
	}
}
