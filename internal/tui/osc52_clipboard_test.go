package tui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// envOf returns an env lookup func backed by a map, standing in for os.Getenv.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestOSC52SequencesPlain pins the exact bytes of the raw OSC 52 clipboard
// sequence: ESC ] 52 ; c ; <base64(StdEncoding)> BEL. This is what a remote
// (SSH) session sends down the pty so the user's local terminal populates the
// local clipboard.
func TestOSC52SequencesPlain(t *testing.T) {
	const sample = "hello, rysh"
	got := OSC52Sequences(envOf(map[string]string{"TERM": "xterm-256color"}), sample)
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(sample)) + "\a"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("OSC52Sequences(plain) =\n  %q\nwant\n  [%q]", got, want)
	}
	// BEL terminator, not ST.
	if got[0][len(got[0])-1] != '\a' {
		t.Fatalf("plain sequence terminator = %q, want BEL (\\a)", got[0][len(got[0])-1])
	}
}

// TestOSC52SequencesTmux: under tmux ($TMUX set) BOTH a raw sequence (for
// set-clipboard on/external, the default) and a passthrough-wrapped one (for
// set-clipboard off + allow-passthrough on) must be emitted. $TMUX must win
// over the screen-looking TERM that tmux sets by default.
func TestOSC52SequencesTmux(t *testing.T) {
	const sample = "tmux copy"
	env := envOf(map[string]string{
		"TMUX": "/tmp/tmux-1000/default,1234,0",
		"TERM": "screen-256color", // tmux default TERM — must NOT select ScreenMode
	})
	got := OSC52Sequences(env, sample)
	b64 := base64.StdEncoding.EncodeToString([]byte(sample))
	wantRaw := "\x1b]52;c;" + b64 + "\a"
	wantWrapped := "\x1bPtmux;\x1b\x1b]52;c;" + b64 + "\a\x1b\\"
	if len(got) != 2 || got[0] != wantRaw || got[1] != wantWrapped {
		t.Fatalf("OSC52Sequences(tmux) =\n  %q\nwant\n  [%q %q]", got, wantRaw, wantWrapped)
	}
}

// TestOSC52SequencesScreen: GNU screen eats raw OSC 52, so the sequence must
// arrive wrapped in a DCS envelope that screen forwards to the outer terminal.
func TestOSC52SequencesScreen(t *testing.T) {
	const sample = "screen copy"
	got := OSC52Sequences(envOf(map[string]string{"TERM": "screen"}), sample)
	b64 := base64.StdEncoding.EncodeToString([]byte(sample))
	want := "\x1bP\x1b]52;c;" + b64 + "\a\x1b\\"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("OSC52Sequences(screen) =\n  %q\nwant\n  [%q]", got, want)
	}
}

// TestOSC52SequencesScreenChunks: screen truncates DCS payloads longer than
// ~768 bytes, so long selections must be split into multiple DCS chunks.
func TestOSC52SequencesScreenChunks(t *testing.T) {
	sample := strings.Repeat("long selection over ssh + screen\n", 20)
	got := OSC52Sequences(envOf(map[string]string{"TERM": "screen-256color"}), sample)
	if len(got) != 1 {
		t.Fatalf("OSC52Sequences(screen, long) returned %d sequences, want 1", len(got))
	}
	// Chunk join marker: end-DCS immediately followed by start-DCS.
	if !strings.Contains(got[0], "\x1b\\\x1bP") {
		t.Fatalf("long screen sequence not DCS-chunked:\n  %q", got[0])
	}
}

// TestWriteOSC52 verifies WriteOSC52 emits the concatenated sequence bytes to
// an arbitrary writer.
func TestWriteOSC52(t *testing.T) {
	const sample = "multi\nline\tselection with spaces"
	var buf bytes.Buffer
	env := envOf(map[string]string{"TERM": "xterm-256color"})
	if err := WriteOSC52(&buf, env, sample); err != nil {
		t.Fatalf("WriteOSC52 error: %v", err)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(sample)) + "\a"
	if buf.String() != want {
		t.Fatalf("WriteOSC52 wrote\n  %q\nwant\n  %q", buf.String(), want)
	}
}
