// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestBuildConcatFileUsesRealGaps is the core of wall-clock-accurate playback:
// because ticks are dropped whenever a capture is still outstanding, frames are
// NOT evenly spaced, so each frame's duration must come from the gap to the
// next capture rather than from the nominal interval. A frame that stayed on
// screen for 2s must occupy 2s of video.
func TestBuildConcatFileUsesRealGaps(t *testing.T) {
	frames := []recFrame{
		{name: "frame-000000.jpg", at: 0},
		{name: "frame-000001.jpg", at: 500 * time.Millisecond},
		{name: "frame-000002.jpg", at: 2500 * time.Millisecond}, // 3 ticks dropped
	}
	got := buildConcatFile(frames, 500*time.Millisecond)

	if !strings.HasPrefix(got, "ffconcat version 1.0\n") {
		t.Errorf("missing concat header: %q", got)
	}
	for _, want := range []string{
		"file 'frame-000000.jpg'\nduration 0.500\n",
		"file 'frame-000001.jpg'\nduration 2.000\n", // the real 2s gap, not 0.5s
	} {
		if !strings.Contains(got, want) {
			t.Errorf("concat file missing %q:\n%s", want, got)
		}
	}
	// The last frame gets the nominal interval (no next frame to measure to)
	// and is then repeated — the concat demuxer drops the final entry's
	// duration, so without the repeat the last frame would flash by.
	if !strings.Contains(got, "file 'frame-000002.jpg'\nduration 0.500\nfile 'frame-000002.jpg'\n") {
		t.Errorf("last frame not repeated with interval duration:\n%s", got)
	}
	if n := strings.Count(got, "frame-000002.jpg"); n != 2 {
		t.Errorf("last frame should appear exactly twice, got %d", n)
	}
}

// TestBuildConcatFileEdgeCases guards the degenerate inputs.
func TestBuildConcatFileEdgeCases(t *testing.T) {
	if got := buildConcatFile(nil, time.Second); got != "ffconcat version 1.0\n" {
		t.Errorf("no frames should yield a bare header, got %q", got)
	}
	// A single frame is still a valid one-frame video.
	got := buildConcatFile([]recFrame{{name: "f.jpg", at: 0}}, 500*time.Millisecond)
	if strings.Count(got, "file 'f.jpg'") != 2 || !strings.Contains(got, "duration 0.500") {
		t.Errorf("single frame concat wrong:\n%s", got)
	}
	// Non-monotonic timestamps must not produce a negative duration (ffmpeg
	// rejects those outright).
	got = buildConcatFile([]recFrame{
		{name: "a.jpg", at: 2 * time.Second},
		{name: "b.jpg", at: time.Second},
	}, 500*time.Millisecond)
	if strings.Contains(got, "duration -") {
		t.Errorf("negative duration emitted:\n%s", got)
	}
}

// TestFrameExtDetectsFromBytes covers graceful degradation: a peer that
// predates the format/quality params (the desktop app, the extension) returns
// PNG no matter what the recorder asked for, so the extension must follow the
// bytes rather than the request.
func TestFrameExtDetectsFromBytes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png magic", []byte("\x89PNG\r\n\x1a\n\x00\x00"), "png"},
		{"jpeg magic", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, "jpg"},
		{"webp magic", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "webp"},
		{"unknown falls back", []byte("not an image"), "jpg"},
		{"empty falls back", nil, "jpg"},
		// A short buffer that starts like RIFF must not panic on the 8:12 slice.
		{"truncated riff", []byte("RIFF"), "jpg"},
	}
	for _, tc := range cases {
		if got := frameExt(tc.data, "jpg"); got != tc.want {
			t.Errorf("%s: frameExt = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFFmpegArgs pins the flags the encode depends on: concat demuxing with
// -safe 0 (the list holds bare filenames), even-dimension scaling (yuv420p
// cannot represent odd sizes, and browser viewports often are), and the output
// path last.
func TestFFmpegArgs(t *testing.T) {
	args := ffmpegArgs("/tmp/x.frames/frames.txt", "/tmp/out.mp4")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-f concat", "-safe 0", "-i /tmp/x.frames/frames.txt", "-pix_fmt yuv420p", "trunc(iw/2)*2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ffmpeg args missing %q: %s", want, joined)
		}
	}
	if args[len(args)-1] != "/tmp/out.mp4" {
		t.Errorf("output path must be last, got %q", args[len(args)-1])
	}
	if args[0] != "-y" {
		t.Errorf("-y (overwrite) must come first, got %q", args[0])
	}
}

// TestLastLines keeps ffmpeg's failure output short enough for a pane line.
func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\nd\n", 2); got != "c | d" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("solo", 3); got != "solo" {
		t.Errorf("lastLines short input = %q", got)
	}
}

// TestRecorderKeepsLateFrames is the regression test for a recorder that threw
// away work the browser had already done.
//
// Capture is drop-don't-queue, and a stall guard stops the recorder waiting on
// a screenshot that takes more than a few intervals so a dead peer cannot wedge
// the run. Giving up on the WAIT used to mean discarding the ANSWER: the frame
// was matched against the single outstanding request id, which the guard had
// already cleared. On a machine where every screenshot ran longer than the
// guard's window — a loaded one, which is exactly when a long automation is
// worth recording — every capture succeeded, every one arrived a moment late,
// and the run reported "no frames captured".
//
// A late screenshot is a real picture of the page at the moment it was asked
// for. It is kept, and stamped with its REQUEST time so it lands at the right
// point in the video rather than bunched against whatever came back next.
func TestRecorderKeepsLateFrames(t *testing.T) {
	r := newFrameTestRecorder(t)

	// Two captures issued back to back, the first stalled out before either
	// answers — the shape a slow machine produces on every single frame.
	first := r.issueCapture(10 * time.Millisecond)
	r.inFlight = "" // the stall guard gave up waiting on `first`
	r.timedOut++
	second := r.issueCapture(300 * time.Millisecond)

	// The answers arrive out of order, the abandoned one last.
	r.handleFrame(fakeScreenshot(second))
	r.handleFrame(fakeScreenshot(first))

	if len(r.frames) != 2 {
		t.Fatalf("kept %d frames, want 2 — a late frame is still a frame", len(r.frames))
	}
	// Stamped by request time, so ordering by `at` restores real chronology
	// even though the answers came back the other way round.
	var earliest, latest time.Duration
	earliest, latest = r.frames[0].at, r.frames[0].at
	for _, f := range r.frames {
		if f.at < earliest {
			earliest = f.at
		}
		if f.at > latest {
			latest = f.at
		}
	}
	if earliest != 10*time.Millisecond || latest != 300*time.Millisecond {
		t.Errorf("frame timestamps = [%v, %v], want [10ms, 300ms] — frames must carry "+
			"their request time, not their arrival time", earliest, latest)
	}
	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 — these captures succeeded, just slowly", r.failed)
	}
}

// TestRecorderIgnoresForeignAndDuplicateFrames keeps the widened matching
// honest: accepting late frames must not mean accepting anything at all. The
// browser_action tool shares this response subject, and a duplicate delivery
// must not be counted twice.
func TestRecorderIgnoresForeignAndDuplicateFrames(t *testing.T) {
	r := newFrameTestRecorder(t)
	id := r.issueCapture(0)

	// Another component's browser_action response on the same subject.
	r.handleFrame(fakeScreenshot("browser-action-42"))
	if len(r.frames) != 0 {
		t.Fatalf("kept a response that was not ours: %+v", r.frames)
	}

	r.handleFrame(fakeScreenshot(id))
	r.handleFrame(fakeScreenshot(id)) // redelivery
	if len(r.frames) != 1 {
		t.Errorf("kept %d frames from one capture, want 1", len(r.frames))
	}
}

// TestRecorderPrunesAbandonedCaptures bounds the memory a peer that answers
// nothing can cost: ids stay claimable long enough to accept a very late
// answer, then are forgotten.
func TestRecorderPrunesAbandonedCaptures(t *testing.T) {
	r := newFrameTestRecorder(t)
	retention := sentAtRetention(r.spec.Interval)
	stale := r.issueCapture(0)
	fresh := r.issueCapture(retention * 3 / 2)

	// Rewind the start so "now" is 2x the retention window in: the cutoff
	// lands at 1x, leaving the first capture clearly outside it and the second
	// clearly inside. Neither sits on the boundary, where a few microseconds
	// of clock drift would decide the result.
	r.startedAt = time.Now().Add(-2 * retention)
	r.pruneSentAt()

	if _, ok := r.sentAt[stale]; ok {
		t.Error("an abandoned capture was never forgotten — sentAt grows without bound")
	}
	if _, ok := r.sentAt[fresh]; !ok {
		t.Error("a recent capture was pruned — its answer would be discarded")
	}
}

// newFrameTestRecorder builds a recorder wired to a scratch frames dir, with
// no bus and no ticker — enough to exercise capture bookkeeping directly.
func newFrameTestRecorder(t *testing.T) *WebRecorderActor {
	t.Helper()
	return &WebRecorderActor{
		paneID:    "p1",
		framesDir: t.TempDir(),
		startedAt: time.Now(),
		reqPrefix: "rec-p1-1-",
		spec:      webauto.RecordSpec{Interval: 200 * time.Millisecond, Format: "jpeg"},
	}
}

// issueCapture records a request as if capture() had sent it at the given
// offset from the start of recording, and returns its id.
func (r *WebRecorderActor) issueCapture(at time.Duration) string {
	r.reqSeq++
	id := r.reqPrefix + fmt.Sprint(r.reqSeq)
	if r.sentAt == nil {
		r.sentAt = make(map[string]time.Duration, 4)
	}
	r.sentAt[id] = at
	r.inFlight = id
	r.inFlightAt = time.Now()
	return id
}

// fakeScreenshot is a successful response carrying one minimal JPEG.
func fakeScreenshot(requestID string) *msg.MsgBrowserActionResponse {
	// SOI + EOI is enough for frameExt to identify it as a JPEG.
	return &msg.MsgBrowserActionResponse{
		RequestID:  requestID,
		Success:    true,
		Screenshot: base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xD9}),
	}
}
