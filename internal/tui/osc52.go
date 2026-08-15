// SPDX-License-Identifier: Apache-2.0

package tui

// OSC 52 clipboard emission.
//
// OSC 52 asks the terminal EMULATOR that owns the display — not the host rysh
// runs on — to write text to its clipboard. That is what makes mouse-copy work
// when rysh runs on a remote host over SSH: the sequence travels down the pty
// to the user's local terminal, which owns the clipboard. Host-side clipboard
// helpers (pbcopy/xclip/wl-copy) only ever reach the machine rysh runs on, so
// they are kept solely as a best-effort local fallback.

import (
	"io"
	"os"
	"strings"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
)

// OSC52Sequences returns the escape sequence(s) that copy s to the clipboard
// of the terminal that owns the display, adapted to the terminal chain
// described by env (os.Getenv in production):
//
//   - Plain terminal: one raw OSC 52 sequence.
//   - GNU screen (TERM=screen*): OSC 52 wrapped in DCS chunks — screen
//     forwards DCS payloads to the outer terminal unchanged but eats raw
//     OSC 52.
//   - tmux ($TMUX set, or TERM=tmux*): BOTH a raw sequence and a
//     tmux-passthrough-wrapped one. Raw works when tmux's set-clipboard is
//     "on" or "external" (the default); the wrapped copy works when
//     set-clipboard is off but allow-passthrough is on. Whichever tmux config
//     is in effect drops the sequence it doesn't understand; if both get
//     through, the terminal just sets the same clipboard content twice.
//
// $TMUX wins over a screen-looking TERM because tmux sets TERM=screen* by
// default.
func OSC52Sequences(env func(string) string, s string) []string {
	term := strings.ToLower(env("TERM"))
	switch {
	case env("TMUX") != "" || strings.HasPrefix(term, "tmux"):
		return []string{
			osc52.New(s).String(),
			osc52.New(s).Tmux().String(),
		}
	case strings.HasPrefix(term, "screen"):
		return []string{osc52.New(s).Screen().String()}
	}
	return []string{osc52.New(s).String()}
}

// WriteOSC52 emits the OSC 52 clipboard sequence(s) for s to w, adapted via
// env to the terminal chain. Split out so the exact bytes are unit-testable.
func WriteOSC52(w io.Writer, env func(string) string, s string) error {
	for _, seq := range OSC52Sequences(env, s) {
		if _, err := io.WriteString(w, seq); err != nil {
			return err
		}
	}
	return nil
}

// OSC52Output returns the writer OSC 52 sequences should be sent to, plus a
// cleanup func. It prefers the controlling terminal (/dev/tty) over os.Stdout:
// a direct stdout write can interleave with a partially-flushed bubbletea
// frame (corrupting the escape sequence mid-bytes), and goes nowhere useful
// when stdout is redirected. Falls back to os.Stdout when there is no
// controlling tty (e.g. Windows).
func OSC52Output() (io.Writer, func()) {
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		return tty, func() { _ = tty.Close() }
	}
	return os.Stdout, func() {}
}
