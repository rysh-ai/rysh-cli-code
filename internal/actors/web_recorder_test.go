package actors

import (
	"strings"
	"testing"
	"time"
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
