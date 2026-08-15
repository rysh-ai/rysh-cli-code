// SPDX-License-Identifier: Apache-2.0

package voice

import "testing"

// TestAudioContentType covers the extension → MIME mapping used for Deepgram
// uploads: the TUI recorder's WAV stays the default; browser MediaRecorder
// containers (web voice, roadmap W10) are declared correctly.
func TestAudioContentType(t *testing.T) {
	cases := map[string]string{
		"/tmp/a.wav":     "audio/wav",
		"/tmp/a.WAV":     "audio/wav",
		"/tmp/rec.webm":  "audio/webm",
		"/tmp/rec.ogg":   "audio/ogg",
		"/tmp/rec.mp4":   "audio/mp4",
		"/tmp/rec.m4a":   "audio/mp4",
		"/tmp/rec.mp3":   "audio/mpeg",
		"/tmp/noext":     "audio/wav",
		"/tmp/rec.other": "audio/wav",
	}
	for path, want := range cases {
		if got := audioContentType(path); got != want {
			t.Errorf("audioContentType(%q) = %q, want %q", path, got, want)
		}
	}
}
