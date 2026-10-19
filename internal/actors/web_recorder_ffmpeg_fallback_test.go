// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// F-82 part b: -fps_mode exists only from ffmpeg 5.1, and Ubuntu 22.04 ships
// 4.4.2. When ffmpeg answers "Unrecognized option 'fps_mode'", encode retries
// ONCE with -vsync vfr. These tests drive encode() against a fake ffmpeg put
// first on PATH, which logs every invocation's argv.

// fakeFFmpeg installs a shell-script ffmpeg as the only thing on PATH and
// returns the file its invocations are logged to, one argv per line. body
// runs after the argv is logged, with $fps = 1 when -fps_mode was passed and
// $last = the last argument (the output path).
func fakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"fps=0; for a; do [ \"$a\" = \"-fps_mode\" ] && fps=1; last=$a; done\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return log
}

const (
	fakeWritesOutput  = `: > "$last"; exit 0`
	fakeRejectsFPS    = `if [ $fps = 1 ]; then echo "Unrecognized option 'fps_mode'." >&2; echo "Error splitting the argument list: Option not found" >&2; exit 8; fi`
	fakeBadInput      = `echo "frames.txt: Invalid data found when processing input" >&2; exit 1`
	fakeUnknownCodec  = `echo "Unknown encoder 'libx264'" >&2; exit 1`
	fakeFrameFileName = "frame-0001.jpg"
)

// newEncodeTestRecorder is a recorder with one frame on disk and an output
// path in a fresh directory: the smallest state encode() runs on.
func newEncodeTestRecorder(t *testing.T) *WebRecorderActor {
	t.Helper()
	r := &WebRecorderActor{
		paneID:    "p1",
		framesDir: t.TempDir(),
		outPath:   filepath.Join(t.TempDir(), "run.mp4"),
		spec:      webauto.RecordSpec{Interval: 200 * time.Millisecond, Format: "jpeg"},
		frames:    []recFrame{{name: fakeFrameFileName, at: 0}},
	}
	if err := os.WriteFile(filepath.Join(r.framesDir, fakeFrameFileName), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func fakeCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// assertNoPart checks the hidden .part encode target never outlives encode().
func assertNoPart(t *testing.T, r *WebRecorderActor) {
	t.Helper()
	part := filepath.Join(filepath.Dir(r.outPath), ".part-"+filepath.Base(r.outPath))
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("%s left behind", part)
	}
}

// (a) ffmpeg 4.x: -fps_mode is rejected, the one -vsync retry encodes.
func TestEncodeFallsBackToVsyncOnFFmpeg4(t *testing.T) {
	log := fakeFFmpeg(t, fakeRejectsFPS+"\n"+fakeWritesOutput)
	r := newEncodeTestRecorder(t)
	err := r.encode()
	calls := fakeCalls(t, log)
	if err != nil {
		t.Fatalf("encode: %v (calls: %q)", err, calls)
	}
	if len(calls) != 2 {
		t.Fatalf("ffmpeg ran %d times, want 2 (-fps_mode, then -vsync): %q", len(calls), calls)
	}
	if !strings.Contains(calls[0], "-fps_mode vfr") {
		t.Errorf("first call %q lacks -fps_mode vfr", calls[0])
	}
	if !strings.Contains(calls[1], "-vsync vfr") || strings.Contains(calls[1], "-fps_mode") {
		t.Errorf("retry %q: want -vsync vfr and no -fps_mode", calls[1])
	}
	if _, err := os.Stat(r.outPath); err != nil {
		t.Errorf("no video at outPath after the retry: %v", err)
	}
	assertNoPart(t, r)
}

// (b) ffmpeg >= 5.1: one call, no probe, no retry.
func TestEncodeNoRetryWhenFpsModeWorks(t *testing.T) {
	log := fakeFFmpeg(t, fakeWritesOutput)
	r := newEncodeTestRecorder(t)
	if err := r.encode(); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if calls := fakeCalls(t, log); len(calls) != 1 || !strings.Contains(calls[0], "-fps_mode vfr") {
		t.Errorf("calls = %q, want exactly one, with -fps_mode vfr", calls)
	}
	if _, err := os.Stat(r.outPath); err != nil {
		t.Errorf("no video at outPath: %v", err)
	}
	assertNoPart(t, r)
}

// (c) Any other failure is returned as it was, without a retry.
func TestEncodeNoRetryOnOtherFailure(t *testing.T) {
	log := fakeFFmpeg(t, fakeBadInput)
	r := newEncodeTestRecorder(t)
	err := r.encode()
	if err == nil {
		t.Fatal("encode succeeded against a failing ffmpeg")
	}
	if calls := fakeCalls(t, log); len(calls) != 1 {
		t.Errorf("ffmpeg ran %d times, want 1 (no retry): %q", len(calls), calls)
	}
	if !strings.Contains(err.Error(), "Invalid data found when processing input") {
		t.Errorf("error %q lost the original ffmpeg output", err)
	}
	if strings.Contains(err.Error(), "-vsync") {
		t.Errorf("error %q claims a -vsync retry that should not have happened", err)
	}
	if _, err := os.Stat(r.outPath); !os.IsNotExist(err) {
		t.Errorf("outPath exists after a failed encode")
	}
	assertNoPart(t, r)
}

// Both attempts fail: the error is the retry's and names both flags.
func TestEncodeRetryAlsoFails(t *testing.T) {
	log := fakeFFmpeg(t, fakeRejectsFPS+"\n"+fakeUnknownCodec)
	r := newEncodeTestRecorder(t)
	err := r.encode()
	if err == nil {
		t.Fatal("encode succeeded though both attempts failed")
	}
	if calls := fakeCalls(t, log); len(calls) != 2 {
		t.Errorf("ffmpeg ran %d times, want 2: %q", len(calls), calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Unknown encoder 'libx264'") {
		t.Errorf("error %q is not the retry's", msg)
	}
	if strings.Contains(msg, "fps_mode'") {
		t.Errorf("error %q carries the first attempt's output, want the retry's", msg)
	}
	if !strings.Contains(msg, "-fps_mode") || !strings.Contains(msg, "-vsync") {
		t.Errorf("error %q does not say both flags were tried", msg)
	}
	if _, err := os.Stat(r.outPath); !os.IsNotExist(err) {
		t.Errorf("outPath exists after a failed encode")
	}
	assertNoPart(t, r)
}

func TestVsyncFallbackArgs(t *testing.T) {
	args := ffmpegArgs("/tmp/x.frames/frames.txt", "/tmp/out.mp4")
	orig := slices.Clone(args)
	got := vsyncFallbackArgs(args)
	if !slices.Equal(args, orig) {
		t.Errorf("vsyncFallbackArgs mutated its input: %v", args)
	}
	i := slices.Index(got, "-vsync")
	if i < 0 || i+1 >= len(got) || got[i+1] != "vfr" || slices.Contains(got, "-fps_mode") {
		t.Fatalf("fallback args %v: want the pair -vsync vfr and no -fps_mode", got)
	}
	// Exactly that one element differs.
	if len(got) != len(args) {
		t.Fatalf("fallback changed the argv length: %v", got)
	}
	for j := range got {
		if j != i && got[j] != args[j] {
			t.Errorf("fallback changed arg %d: %q -> %q", j, args[j], got[j])
		}
	}
}
