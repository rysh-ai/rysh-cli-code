// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (g1): resuming a replay after a daemon restart must read the WHOLE
// recording. Timeline prefers the RAM ring and reads the durable stream only
// when the ring is empty — but by the time a restart restores the replay pane,
// the source pane has usually printed again (a prompt redraw), so the ring
// holds only post-restart output and Timeline would replay the wrong thing.
// ResumeTimeline prefers the stream, which holds both.
func TestResumeTimelinePrefersDurableStream(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	const session = "resume-tl-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// Before the restart: the old daemon's capture wrote to the stream.
	before := NewCapture(nc, codecs, session)
	if err := before.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	publishAppend(t, pub, "p1", "one\n", 1000)
	publishAppend(t, pub, "p1", "two\n", 2000)
	waitStreamMsgs(t, nc, StreamName(session), 2)

	// After the restart: a fresh capture whose RAM ring sees only new output.
	after := NewCapture(nc, codecs, session)
	if err := after.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(after.Stop)
	if err := after.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	publishAppend(t, pub, "p1", "prompt\n", 9000)
	waitStreamMsgs(t, nc, StreamName(session), 3)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && after.Count("p1") < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	if tl, _, _ := after.Timeline("p1"); len(tl) != 1 {
		t.Fatalf("setup: Timeline = %d events, want the 1 post-restart event (RAM ring wins)", len(tl))
	}
	tl, start, err := after.ResumeTimeline("p1")
	if err != nil {
		t.Fatalf("ResumeTimeline: %v", err)
	}
	if len(tl) != 3 || tl[0].Text != "one\n" || tl[2].Text != "prompt\n" {
		t.Fatalf("ResumeTimeline = %+v, want the whole recording: one, two, prompt", tl)
	}
	if !start.Equal(time.UnixMilli(1000)) {
		t.Fatalf("start = %v, want the first pre-restart event's time", start)
	}
}
