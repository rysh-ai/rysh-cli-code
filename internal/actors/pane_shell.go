package actors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/vterm"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/platform"
)

// Raw-share publish coalescing (rawReadLoop). A full-screen interactive program
// emits a ~4KB PTY chunk per read at PTY I/O frequency; publishing each one as a
// separate shared raw message floods every subscriber. We coalesce: buffer chunk
// bytes and flush when the buffer reaches rawPublishMaxBytes OR rawPublishMaxDelay
// has elapsed since the last flush. Bytes are never dropped or reordered.
const (
	rawPublishMaxBytes = 32 * 1024 // flush when this many bytes are buffered
	rawPublishMaxDelay = 16 * time.Millisecond
	// rawPublishIdleFlush bounds how long buffered-but-unflushed raw bytes may sit
	// when no NEW PTY chunk arrives to drive the size/delay flush. An interactive
	// program like claude redraws only on input, so its final per-redraw chunk
	// (echoed char + cursor) would otherwise stay buffered until the next
	// keystroke. We arm a short PTY read deadline to wake the loop and flush it.
	rawPublishIdleFlush = 8 * time.Millisecond
)

// earliestDeadline returns the earlier of two PTY read deadlines, treating a
// zero time.Time as "unset". It is the single arbitration point for the two
// users of the rawReadLoop read deadline — the idle raw-publish flush and the
// cursor-hidden exit debounce — so neither one's clear stomps the other's need.
// The second return value is false when BOTH inputs are unset (read deadline
// should be cleared). Pure (no side effects) so it can be unit-tested directly.
func earliestDeadline(a, b time.Time) (time.Time, bool) {
	switch {
	case a.IsZero() && b.IsZero():
		return time.Time{}, false
	case a.IsZero():
		return b, true
	case b.IsZero():
		return a, true
	case a.Before(b):
		return a, true
	default:
		return b, true
	}
}

// ---------------------------------------------------------------------------
// Shell management
// ---------------------------------------------------------------------------

// paneShellEnv is the daemon's environment minus the RYSH_WEB_* variables that
// configured THIS daemon's web server.
//
// A pane shell inherits the daemon's environment so the user's tools behave as
// they would in any terminal — but the desktop app spawns its daemon with
// RYSH_WEB_CONTROL / RYSH_WEB_AUTO_START / RYSH_WEB_PORT, and those describe one
// specific server, not a preference to pass on. Left in place, a `rysh create`
// typed into a pane inherits them and the new, entirely unrelated daemon comes
// up in control mode, auto-starting a web server aimed at the port the app is
// already listening on. These variables configure a daemon; they have no
// business crossing into the shells that daemon hosts.
func paneShellEnv() []string {
	env := os.Environ()
	kept := env[:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "RYSH_WEB_CONTROL="),
			strings.HasPrefix(kv, "RYSH_WEB_AUTO_START="),
			strings.HasPrefix(kv, "RYSH_WEB_PORT="),
			strings.HasPrefix(kv, "RYSH_WEB_HOST="):
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// paneIdentityEnv is what a program running INSIDE a pane needs in order to
// address the pane it is in.
//
// Without it, such a program can only ask the daemon which pane is ACTIVE — and
// that is wherever the user's focus happens to be, not where the program lives.
// An agent driving its sibling panes therefore followed the user around the
// screen instead of anchoring on itself, and a script could not reliably send
// anything to its own pane.
//
// RYSH_SESSION / RYSH_TAB / RYSH_PANE are the names `rysh script` already
// exports and the prelude already consumes, so `##` routing behaves the same
// whether a script was launched by `rysh script` or typed into a pane.
// RYSH_LANE and RYSH_STACK complete the path, because a selector is only
// unambiguous when it is fully qualified: `--pane` alone is resolved within the
// ACTIVE pane's group, so a program addressing a pane outside that group needs
// every level. Every value is an id, and --tab/--lane/--pg/--pane all resolve an
// id before an index or a name.
//
// "stack" rather than "group" or "pg" for the pane group: it is the word the
// commands use (`##new stack`, `##cmd stack`), and a pane group is exactly a
// stack of panes.
//
// All five are fixed for the pane's lifetime — a pane keeps its id and is
// created inside one tab, lane and stack rather than migrating between them.
func paneIdentityEnv(session, tabID, laneID, stackID, paneID string) []string {
	return []string{
		"RYSH_SESSION=" + session,
		"RYSH_TAB=" + tabID,
		"RYSH_LANE=" + laneID,
		"RYSH_STACK=" + stackID,
		"RYSH_PANE=" + paneID,
	}
}

func (p *PaneActor) startShell() {
	// In interactive mode the shell is launched with -i so it sources the
	// user's rc files (~/.bashrc, ~/.zshrc) — making aliases, functions, shell
	// options, and the user's real prompt available, just like a normal
	// terminal. In legacy mode the shell is launched plainly and rysh forces a
	// minimal "$ " prompt that it then strips from the output.
	var cmd *exec.Cmd
	if p.cfg.InteractiveShell {
		cmd = exec.Command(p.cfg.DefaultShell, "-i")
	} else {
		cmd = exec.Command(p.cfg.DefaultShell)
	}
	cmd.Env = append(paneShellEnv(),
		"BASH_SILENCE_DEPRECATION_WARNING=1",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"COLORFGBG=15;0",
	)
	// Tell the program in the pane WHICH pane it is in. Appended after the
	// inherited environment so a value carried in from an outer session cannot
	// make a pane claim to be its parent.
	cmd.Env = append(cmd.Env,
		paneIdentityEnv(p.cfg.SessionName, p.tabID, p.laneID, p.groupID, p.id)...)
	// Governance proxy (design 001): when the loopback proxy is running, route
	// this pane's third-party-CLI provider traffic through it by base-URL env
	// injection (no TLS MITM). Empty endpoint ⇒ proxy off ⇒ env untouched.
	if base := proxy.Endpoint(); base != "" {
		// Register the pane as a legitimate attribution target before its
		// program can call: the proxy refuses a {paneID} segment it never
		// injected, and this is the moment rysh injects one.
		if srv := proxy.Current(); srv != nil {
			srv.NotePane(p.id)
		}
		cmd.Env = append(cmd.Env,
			"ANTHROPIC_BASE_URL="+base+"/anthropic/"+p.id,
			"OPENAI_BASE_URL="+base+"/openai/"+p.id,
			"GEMINI_BASE_URL="+base+"/gemini/"+p.id,
		)
		if p.cfg.Proxy.InjectKey {
			// Dummy pane-side key; the real key is injected proxy-side.
			//
			// Only for dialects rysh actually HOLDS a key for. Injecting a
			// dummy for a dialect with no real key replaced the user's own
			// working credential with a placeholder that the proxy then had
			// nothing to swap it for — which is exactly how enabling the proxy
			// broke wrapped OpenAI CLIs.
			for _, d := range []struct{ dialect, env string }{
				{"anthropic", "ANTHROPIC_API_KEY"},
				{"openai", "OPENAI_API_KEY"},
				{"gemini", "GEMINI_API_KEY"},
			} {
				if p.cfg.ProxyKeyFor(d.dialect) != "" {
					cmd.Env = append(cmd.Env, d.env+"="+config.ProxyDummyKey)
				}
			}
		}
		// Per-CLI adapters (design 022 §4.4). Base-URL env is not enough for
		// every CLI — codex can fall back to a ChatGPT session and egress
		// around the proxy entirely — so an opted-in CLI gets the extra
		// environment that actually forces it through. Opt-in because the
		// adapter can change how the CLI authenticates.
		cmd.Env = append(cmd.Env, p.proxyAdapterEnv(base)...)
	}
	if p.cfg.InteractiveShell && strings.Contains(filepath.Base(p.cfg.DefaultShell), "bash") {
		// OSC 7 cwd reporting: bash emits its working directory before every
		// prompt; rawReadLoop parses it (osc7Scanner) so tab-completion and
		// the shell prompt always know the live cwd — push-based and exact,
		// unlike the lsof/proc polling fallback. Best-effort: a bashrc that
		// overwrites PROMPT_COMMAND silently disables it (fallback remains).
		cmd.Env = append(cmd.Env,
			`PROMPT_COMMAND=printf '\033]7;file://%s%s\033\\' "$HOSTNAME" "$PWD"`)
	}
	if !p.cfg.InteractiveShell {
		// Legacy: force a minimal prompt because rysh synthesizes its own
		// "$ <cmd>" echo and strips PTY prompt lines. In interactive mode we
		// leave PS1 untouched so the user's real prompt shows through.
		cmd.Env = append(cmd.Env, "PS1=$ ")
	}

	// Start the pane's shell in the configured working directory. If the
	// directory is unset or invalid, fall back to the daemon's working
	// directory (cmd.Dir == "").
	if wd := strings.TrimSpace(p.cfg.WorkingDirectory); wd != "" {
		if info, err := os.Stat(wd); err == nil && info.IsDir() {
			cmd.Dir = wd
			// Also export PWD=wd so the shell reports the CONFIGURED path, not the
			// symlink-resolved one. cmd.Dir performs a chdir, whose physical cwd is
			// the resolved target (e.g. "rysh-ai" -> "agentic-zellij"); without a
			// matching PWD, bash recomputes it via getcwd() and shows the target.
			// Setting PWD to wd mirrors how an interactive `cd <symlink>` keeps the
			// logical path. Appended last so it overrides the inherited PWD.
			cmd.Env = append(cmd.Env, "PWD="+wd)
		}
	}

	if !platform.PTYSupported {
		// Reachable only for a source build on an unsupported host — the CLI
		// preflight (requirePTY) refuses session-opening commands earlier. Say
		// why, rather than surfacing a bare "operation not supported".
		errText := "cannot open a shell: " + platform.PTYUnsupportedReason +
			"\nUse WSL2 on Windows; see docs/wsl.md.\n"
		p.appendOutput(errText)
		p.appendPrivateBuffer(errText)
		p.status = "error"
		return
	}

	f, err := pty.Start(cmd)
	if err != nil {
		errText := fmt.Sprintf("failed to start shell: %v\n", err)
		p.appendOutput(errText)
		p.appendPrivateBuffer(errText)
		p.status = "error"
		return
	}
	p.ptyFile = f
	p.cmd = cmd
	if cmd.Process != nil {
		// pty.Start runs the shell with Setsid, so it leads its own process
		// group. Record it to detect when foreground commands are running.
		p.shellPgid = processPgid(cmd.Process.Pid)
		// Publish the PID for the agentic cwd resolver (lock-free cross-goroutine
		// read) so AI-mode tools can follow the shell's cwd after a `cd`.
		p.shellPIDAtomic.Store(int64(cmd.Process.Pid))
	}

	// Initialize PTY size (default 80x24, will be updated by TUI resize).
	p.ptyRows = 24
	p.ptyCols = 80
	_ = pty.Setsize(f, &pty.Winsize{Rows: p.ptyRows, Cols: p.ptyCols})

	// Initialize virtual terminal emulator.
	p.vtermEmu = vterm.NewWithWriter(int(p.ptyRows), int(p.ptyCols), terminalResponseWriter{
		paneID: p.id,
		w:      f,
	})
	p.seedVTermCursorToBottom("start")

	// Subscribe to raw key input data-plane bypass subject.
	rawSubject := msg.T("pane", p.id, "rawinput")
	p.rawInputSub, _ = p.nc.Subscribe(rawSubject, func(natMsg *nats.Msg) {
		// Decode the NATSEnvelope to extract raw key data.
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		if env.TypeTag != msg.TagRawKeyInput {
			return
		}
		var m msg.MsgRawKeyInput
		if err := json.Unmarshal(env.Payload, &m); err != nil {
			return
		}
		if p.ptyFile != nil && len(m.Data) > 0 {
			_, _ = p.ptyFile.Write(m.Data)
		}
	})

	// The foreground poller lives exactly as long as the read loop: the channel
	// is local so a later startShell cannot race with this one's goroutines.
	foregroundStop := make(chan struct{})
	go p.watchForeground(f, foregroundStop)
	go func() {
		p.rawReadLoop(f)
		close(foregroundStop)
	}()
}

func (p *PaneActor) stopShell() {
	if p.rawInputSub != nil {
		_ = p.rawInputSub.Unsubscribe()
		p.rawInputSub = nil
	}
	// Clear relay state.
	p.relayActive.Store(false)
	if p.ptyFile != nil {
		_ = p.ptyFile.Close()
		p.ptyFile = nil
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	// Clear the published PID so the cwd resolver falls back to the startup dir
	// rather than probing a dead (or recycled) PID.
	p.shellPIDAtomic.Store(0)
}

// seedVTermCursorToBottom starts a fresh VTerm where a real terminal normally
// is after the user has been typing: with the cursor on the last row. Inline
// TUIs such as Claude/Bubble Tea render from the shell cursor position, so a
// fresh row-0 cursor makes them appear pinned to the top of stacked panes.
//
// This writes only to the emulator, not the PTY, so the child shell does not
// receive synthetic input. The raw read loop will consume the shell prompt
// after this seed and render it at the bottom of the VTerm.
//
// After moving the cursor, it also saves the cursor position (SCOSC / ESC[s)
// so that if any program triggers a cursor-restore (SCORC / ESC[u) without
// a prior explicit save, the restore lands at the bottom rather than at the
// default origin (0,0).
func (p *PaneActor) seedVTermCursorToBottom(reason string) {
	if p.vtermEmu == nil || p.ptyRows <= 1 {
		return
	}
	row, _ := p.vtermEmu.CursorPos()
	target := int(p.ptyRows) - 1
	if row >= target {
		return
	}
	_, _ = p.vtermEmu.Write(bytes.Repeat([]byte{'\n'}, target-row))
	// Save cursor position so any accidental SCORC (ESC[u) restores to the
	// bottom rather than the default origin (0,0). This is a safety net for
	// escape sequences that slip past the CSI filter in vterm.Write().
	_, _ = p.vtermEmu.Write([]byte("\x1b[s"))
}

type terminalResponseWriter struct {
	paneID string
	w      io.Writer
}

// clearStaleMouseModes drops mouse tracking left behind by a child that just
// exited. fg is the PTY's new foreground process group, and the reset happens
// only when that is the pane's own shell again — the moment the child is gone.
//
// The emulator lives as long as the pane, so a program that enables tracking
// and dies without disabling it (Ctrl+C, a crash, a sloppy teardown) otherwise
// leaves the bit set for good, and every LATER program in the pane looks like
// it wants mouse reports. The TUI then forwards wheel and click events into a
// PTY that never asked for them, and Claude Code or a bare shell renders those
// as literal "<65;50;54M" text.
//
// Gating on the return to the shell rather than on any foreground change lets a
// program keep its tracking across a shell-out: vim's :!cmd runs in a NEW
// process group, never the pane's shell pgid, so it does not trip this. Ctrl+Z
// does hand the terminal back to the shell and so does clear the suspended
// program's tracking, which is the right trade: programs re-initialise the
// terminal (mouse tracking included) when they resume, and a suspended one is
// indistinguishable from an exited one until it does.
//
// Ordering is safe against a program that starts and immediately enables
// tracking: the kernel sets the foreground group before the new program can
// write, so its \x1b[?1002h always arrives in a chunk read after this reset,
// never in one clobbered by it.
func (p *PaneActor) clearStaleMouseModes(fg int) {
	if p.vtermEmu == nil || p.shellPgid <= 0 || fg != p.shellPgid {
		return
	}
	p.vtermEmu.ResetMouseModes()
}

// rawReadLoop reads from the PTY in byte chunks (not line-based) and feeds
// the data into the VT emulator. It also publishes stripped output for the
// shared output actor and the regular text buffer.
//
// When relay mode is active (p.relayActive), raw PTY bytes are published to a
// dedicated NATS subject (rysh.pane.{id}.relay.data) for the TUI to write
// directly to stdout. This bypasses the snapshot/VTerm rendering path for
// native-speed interactive I/O (vim, htop, claude, etc.).
func (p *PaneActor) rawReadLoop(r io.Reader) {
	buf := make([]byte, 4096)
	pub := p.pub
	id := p.id
	nc := p.nc
	relayFlag := &p.relayActive
	relayDataSubj := msg.T("pane", id, "relay", "data")
	relayExitSubj := msg.T("pane", id, "relay", "exit")

	// applyPromptStrip removes shell prompt lines from buffered output in legacy
	// mode. In interactive mode the user's real prompt is part of the desired
	// output (it's their actual PS1), so it is passed through verbatim.
	// p.cfg is immutable after actor construction, so reading it here is safe.
	interactive := p.cfg.InteractiveShell
	applyPromptStrip := func(s string) string {
		if interactive {
			return s
		}
		return stripShellPrompts(s)
	}

	// colorOutput preserves SGR color/style escapes in the published shell
	// text (see sanitizeShellChunk) so the pane scrollback keeps colors.
	// Interactive mode only: the legacy path does echo/prompt string matching
	// on fully-stripped text and stays plain. p.cfg is immutable.
	colorOutput := interactive && p.cfg.ShellColorOutput

	// suppressBuf accumulates stripped PTY text while echo suppression is
	// active. Kept as a local variable (only rawReadLoop touches it) to
	// avoid data races with the atomic echoSuppress pointer.
	var suppressBuf string

	// pendingTail holds a destructive chunk tail (a bare CR that is really half
	// of a \r\n pair, or an unterminated escape) until the next read completes
	// it. See splitTrailingPartial. Local to this goroutine; flushed on EOF so
	// no output is lost when the shell exits mid-tail.
	var pendingTail string

	// Coalesce the SHARED raw publish (mechanism #1 in the perf root cause): a
	// full-screen TUI emits a 4KB PTY chunk per read at PTY I/O frequency, and
	// SendRawOutput per chunk floods every subscriber. We buffer chunk bytes and
	// flush via SendRawOutput when the buffer reaches rawPublishMaxBytes OR
	// rawPublishMaxDelay has elapsed since the last flush — never dropping or
	// reordering bytes. The LOCAL vterm is still fed every chunk (above); only the
	// shared publish is coalesced. All state is local to this goroutine.
	var rawPubBuf []byte
	var rawPubLastFlush time.Time
	// rawPubFlushDeadline is the time at which buffered-but-unflushed raw bytes
	// must be flushed even if no new PTY chunk arrives (zero = no pending flush).
	// It is one of two users of the PTY read deadline; see the coordination via
	// recomputeReadDeadline below. Local to this goroutine.
	var rawPubFlushDeadline time.Time
	flushRawPub := func() {
		if len(rawPubBuf) > 0 {
			_ = msg.SendRawOutput(pub, id, rawPubBuf)
			rawPubBuf = rawPubBuf[:0]
			// Tell the TUI this pane's local VT screen just changed (mechanism
			// #2 of the perf root cause): the TUI used to poll every visible
			// raw pane's full snapshot at 50ms regardless of whether anything
			// changed. With this push signal, the TUI can fetch only on
			// transitions (and only the panes that signalled). The cadence is
			// the same as the source-side raw publish coalesce (≤16ms / 32KB),
			// which already bounds load even during a full-screen redraw.
			_ = msg.SendPaneRawDirty(pub, id)
		}
		rawPubLastFlush = time.Now()
		// A flush empties the buffer, so any pending idle-flush is satisfied.
		// Clearing here keeps the deadline invariant true at every flush site
		// (size/delay, mode transition, EOF) without each having to repeat it.
		rawPubFlushDeadline = time.Time{}
	}

	// Interactivity is driven by two independent VTerm signals, handled
	// differently:
	//
	//  1. Alternate-screen buffer (vim, less, htop, nano). These programs
	//     toggle the alt screen cleanly exactly once on start and once on
	//     quit, and never flicker it during normal use. Both directions are
	//     therefore applied IMMEDIATELY. Debouncing the exit here is actively
	//     harmful: after the program quits the shell goes idle, so no further
	//     PTY chunks arrive to drive the (event-driven) debounce check — the
	//     pane would stay in raw mode rendering the program's restored screen
	//     (the shell's redrawn PS1) until the next keystroke, instead of
	//     snapping back to the rysh pane prompt.
	//
	//  2. Cursor visibility (inline Bubble Tea apps like Claude Code). These
	//     briefly toggle the cursor (\x1b[?25h then \x1b[?25l) during
	//     re-renders, causing the VTerm to flicker between interactive and
	//     non-interactive states. Without debouncing, syncRawMode() would
	//     oscillate between modeRaw and modeNormal on every snapshot, causing
	//     repeated PTY resizes (overhead 1→4→1→4 rows) and garbled content.
	//     So cursor-hidden ENTRY is immediate, but EXIT is debounced.
	//
	// The debounce MUST be significantly longer than the cursor blink cycle.
	// Bubble Tea's default cursor blink is 530ms (530ms visible / 530ms hidden).
	// During the "cursor visible" phase, any PTY output (animations, status
	// updates) triggers debounce checks. At 500ms debounce vs 530ms blink,
	// the debounce can expire before the cursor re-hides — causing the pane to
	// exit interactive mode, switch to plain-text rendering, and resize the PTY.
	// Using 3 seconds gives ~5× margin over the blink cycle while still
	// detecting program exit within a reasonable delay.
	const interactiveExitDebounce = 3 * time.Second
	var exitDebounceStart time.Time // zero = not debouncing
	debouncing := false

	// While a cursor-hidden exit debounce is armed we set a read deadline on
	// the PTY so the loop wakes up to commit the exit even when the shell has
	// gone idle (no further PTY output to drive the event-driven debounce).
	// Without this, an inline program that quits (e.g. Claude Code, which
	// toggles cursor visibility rather than the alt screen) leaves the pane
	// stuck in raw mode rendering the shell's redrawn PS1 until the next
	// keystroke, instead of snapping back to the rysh pane prompt. The PTY is
	// an *os.File, whose read deadline is supported on Unix; if r does not
	// support it we fall back to the (event-driven) behaviour.
	type readDeadliner interface{ SetReadDeadline(time.Time) error }
	rd, canDeadline := r.(readDeadliner)
	armReadDeadline := func(at time.Time) {
		if canDeadline {
			_ = rd.SetReadDeadline(at)
		}
	}
	clearReadDeadline := func() {
		if canDeadline {
			_ = rd.SetReadDeadline(time.Time{})
		}
	}

	// recomputeReadDeadline is the SINGLE arbitration point for the PTY read
	// deadline shared by its two users: the idle raw-publish flush
	// (rawPubFlushDeadline) and the cursor-hidden exit debounce (active while
	// debouncing). It arms the read deadline at the EARLIEST of the two pending
	// deadlines, or clears it when neither is pending. Call this after either
	// deadline changes so one user's clear never stomps the other's need.
	recomputeReadDeadline := func() {
		var debounceDeadline time.Time
		if debouncing {
			debounceDeadline = exitDebounceStart.Add(interactiveExitDebounce)
		}
		if at, ok := earliestDeadline(rawPubFlushDeadline, debounceDeadline); ok {
			armReadDeadline(at)
		} else {
			clearReadDeadline()
		}
	}

	// Track the foreground program so we can tell a redrawing TUI (whose
	// stripped output is jumbled garbage) from an append-only command (whose
	// output is real scrollback worth keeping). An inline TUI such as codex
	// gives no terminal signal, but it rewrites the screen in place (moves the
	// cursor up), whereas ls/make/cat only append. All state here is local to
	// this goroutine. p.shellPgid is set before the goroutine starts, so
	// reading it is safe; ptyFile is the same *os.File passed in as r.
	ptyFile, _ := r.(*os.File)
	lastFg := -2
	fgRedrew := false

	// OSC 7 cwd tracking: the shell (or any program) reports its working
	// directory through ESC]7;file://host/path — parsed here and mirrored
	// atomically for snapshot reads (tab-completion, shell prompt).
	var osc7 osc7Scanner

	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]

			if cwd, ok := osc7.feed(chunk); ok {
				p.shellCwdAtomic.Store(cwd)
			}

			// Determine the current foreground program; reset the redraw flag
			// whenever it changes (new program, or back to the shell).
			fg := -1
			if ptyFile != nil {
				fg = foregroundPgrp(ptyFile)
			}
			childFg := fg > 0 && p.shellPgid > 0 && fg != p.shellPgid
			if fg != lastFg {
				p.clearStaleMouseModes(fg)
				lastFg = fg
				fgRedrew = false
				// A new foreground program starts a fresh governance
				// observation (design 022 §4.4) — each run of a CLI is judged
				// on its own traffic, not on the previous program's — and is
				// announced to anyone supervising the pane.
				p.noteForegroundChange(fg)
			}
			// Cheap latch check on the same loop the pane already runs — no
			// timer, and the warning lands next to the CLI's own output where
			// the user is looking.
			p.emitUnproxiedWarning()

			// Feed raw bytes into the virtual terminal emulator.
			// VTerm must always see the data so snapshot state stays correct.
			if p.vtermEmu != nil {
				beforeRow, _ := p.vtermEmu.CursorPos()
				_, _ = p.vtermEmu.Write(chunk)
				afterRow, _ := p.vtermEmu.CursorPos()
				if afterRow < beforeRow || (afterRow == 0 && beforeRow > 0) {
					// The cursor moved up: a foreground program rewriting the
					// screen in place (a TUI like codex). Mark it so its stripped
					// output is not appended to the shell buffer as garbage.
					if childFg {
						fgRedrew = true
					}
				}
			}

			// Read the instantaneous VTerm state. These are the "raw" flags
			// from the emulator — we don't use them directly as the pane's
			// interactive state because of debouncing.
			var vtAltScreen, vtCursorHidden bool
			if p.vtermEmu != nil {
				vtAltScreen = p.vtermEmu.IsAltScreen()
				vtCursorHidden = p.vtermEmu.IsCursorHidden()
			}
			// A foreground program that hides the cursor is also a redrawing TUI
			// (e.g. codex). This is tracked per-foreground-program (fgRedrew is
			// reset on every foreground change above), so unlike the debounced
			// p.cursorHidden it never lingers after the program exits.
			if childFg && vtCursorHidden {
				fgRedrew = true
			}

			// wasInteractive is the pane's current (debounced) interactive state.
			wasInteractive := p.rawMode || p.cursorHidden

			// (1) Alt-screen is authoritative and applied IMMEDIATELY in both
			// directions — no debounce. See the rationale near the
			// interactiveExitDebounce declaration above.
			prevAlt := p.rawMode
			p.rawMode = vtAltScreen

			if prevAlt && !vtAltScreen {
				// Alt-screen program just exited. The whole program is gone, so
				// sync cursor visibility immediately and cancel any pending
				// cursor-hidden debounce — there is nothing left to flicker.
				p.cursorHidden = vtCursorHidden
				if debouncing {
					debouncing = false
					recomputeReadDeadline()
				}
			} else if vtCursorHidden {
				// (2) Cursor-hidden interactivity — entry is immediate; cancel
				// any pending exit debounce since interactivity has returned.
				if debouncing {
					debouncing = false
					recomputeReadDeadline()
				}
				p.cursorHidden = true
			} else if p.cursorHidden {
				// (2) Cursor became visible while the pane is cursor-hidden
				// interactive. Debounce the exit to absorb cursor-blink flicker.
				if !debouncing {
					exitDebounceStart = time.Now()
					debouncing = true
					// Arm a read deadline so the exit commits even if the shell
					// goes idle (see readDeadliner note above). Recompute so the
					// debounce deadline coexists with any pending raw-flush one.
					recomputeReadDeadline()
				}
				if time.Since(exitDebounceStart) >= interactiveExitDebounce {
					p.cursorHidden = false
					debouncing = false
					recomputeReadDeadline()
				}
				// else: still debouncing — keep cursorHidden as-is (interactive).
			}

			isInteractive := p.rawMode || p.cursorHidden

			// On interactive mode transitions, notify sharing subscribers
			// BEFORE sending raw bytes so the remote VTerm is created with
			// correct dimensions before it processes any output. When leaving
			// interactive mode, flush any coalesced raw bytes FIRST so the final
			// pre-exit frame reaches subscribers before the mode reset.
			if wasInteractive != isInteractive {
				if wasInteractive && !isInteractive {
					flushRawPub()
				}
				rows := int(p.ptyRows)
				cols := int(p.ptyCols)
				// p.ptyRows/ptyCols are written by handleResize on the actor
				// goroutine and read here on the rawReadLoop goroutine with no
				// synchronisation, so after a resize they can still read the stale
				// 80x24 init value while the (mutex-synced) vtermEmu already holds
				// the real size claude rendered at. Forwarding the stale dims makes
				// the subscriber build an undersized mirror VTerm and wrap-scramble
				// claude's wider output (shell escapes the bug only because short
				// command lines never exceed 80 cols). Read the authoritative size
				// from the vterm so the subscriber's mirror matches the source.
				if isInteractive && p.vtermEmu != nil {
					if vr, vc := p.vtermEmu.Rows(), p.vtermEmu.Cols(); vr > 0 && vc > 0 {
						rows, cols = vr, vc
					}
				}
				if !isInteractive {
					rows, cols = 0, 0
				}
				_ = msg.SendShareModeChange(pub, id, isInteractive, rows, cols)
			}

			// Coalesce the raw bytes for remote sharing when interactive: buffer
			// the chunk and flush when it reaches rawPublishMaxBytes or
			// rawPublishMaxDelay has elapsed since the last flush. Bytes are never
			// dropped or reordered; the local vterm already saw this chunk above.
			if isInteractive {
				rawPubBuf = append(rawPubBuf, chunk...)
				if len(rawPubBuf) >= rawPublishMaxBytes ||
					time.Since(rawPubLastFlush) >= rawPublishMaxDelay {
					flushRawPub()
				}
				// Idle/trailing flush: if bytes remain buffered after the
				// conditional flush, arm a short read deadline so the loop wakes
				// to flush the tail even when NO further PTY chunk arrives (claude
				// redraws only on input — its last per-redraw chunk would
				// otherwise sit here until the next keystroke). If the buffer was
				// flushed empty, drop the pending flush deadline. Either way
				// recompute so this coexists with the exit-debounce deadline.
				if len(rawPubBuf) > 0 {
					rawPubFlushDeadline = time.Now().Add(rawPublishIdleFlush)
				} else {
					rawPubFlushDeadline = time.Time{}
				}
				recomputeReadDeadline()
			}

			// Check relay state (atomic; safe for concurrent read).
			active := relayFlag.Load()
			sentToRelay := false

			if wasInteractive && !isInteractive && active {
				// Transitioning OUT of interactive mode while relay is active.
				// Publish the final chunk then signal the TUI relay to stop.
				_ = nc.Publish(relayDataSubj, chunk)
				_ = nc.Publish(relayExitSubj, nil)
				_ = nc.Flush()
				relayFlag.Store(false)
				sentToRelay = true
				_ = pub.Send(msg.T("pane", id, "status"),
					&msg.MsgPaneStatusUpdate{Status: "idle"})
			} else if active {
				// Normal relay path: publish raw bytes directly via NATS.
				// nc.Publish copies the data, so reusing buf is safe.
				_ = nc.Publish(relayDataSubj, chunk)
				sentToRelay = true
			}

			// Publish status changes for non-relay transitions.
			if !sentToRelay {
				if !wasInteractive && isInteractive {
					_ = pub.Send(msg.T("pane", id, "status"),
						&msg.MsgPaneStatusUpdate{Status: "interactive"})
				} else if wasInteractive && !isInteractive {
					_ = pub.Send(msg.T("pane", id, "status"),
						&msg.MsgPaneStatusUpdate{Status: "idle"})
				}
			}

			// Append stripped text to the shell buffer EXCEPT for a real TUI:
			// an alt-screen program (p.rawMode) or a redrawing foreground TUI
			// (e.g. codex — moves the cursor up or hides it). Their stripped
			// redraw sequences would be garbage in the buffer.
			//
			// Deliberately NOT gated on p.cursorHidden: that flag is debounced
			// and lingers ~3s after an inline TUI exits, which previously made
			// the next command's (e.g. ls) output get suppressed even though the
			// shell was already back at its prompt. redrawingTUI resets the
			// moment the foreground program changes, so it does not linger.
			redrawingTUI := childFg && fgRedrew
			if !p.rawMode && !redrawingTUI {
				// Re-attach the tail held back from the previous read, then hold
				// this read's tail, so a \r\n pair or an escape sequence split
				// across two PTY reads is processed whole. Without this a CRLF
				// split silently deleted the chunk's last line, and a split
				// escape recombined into a live sequence in the output buffer
				// aimed at the user's real terminal.
				// An empty result needs no early exit: the stripped text is then
				// "" and every publish below is already guarded on non-empty.
				chunkText := pendingTail + string(chunk)
				chunkText, pendingTail = splitTrailingPartial(chunkText)
				chunk = []byte(chunkText)

				// Strip ANSI escape sequences only — do NOT call
				// stripShellPrompts per-chunk because its TrimLeft("\n")
				// destroys newlines at chunk boundaries (e.g. PTY
				// splits "ls\r" | "\noutput" → stripping removes the
				// joining "\n" → "ls"+"output" = garbled). Prompt
				// stripping is deferred to publish time below.
				stripped := stripAnsiEscapes(string(chunk))

				// Display text: with color output on, keep SGR sequences and
				// collapse \r rewrites; otherwise display the stripped text.
				// `stripped` still drives redraw detection and (legacy-mode)
				// echo suppression below — those match on plain text.
				display := stripped
				if colorOutput {
					display = sanitizeShellChunk(string(chunk))
				}

				// Skip a pure in-place line redraw (see isLineRedraw): the shell
				// re-issuing its prompt on every SIGWINCH/resize overwrites the
				// current line rather than adding scrollback, so appending it
				// would stack "bash-3.2$ bash-3.2$ ..." across the burst of
				// resizes a freshly-opened pane receives as its layout settles.
				// The VTerm above still consumes the raw bytes, so interactive
				// panes, snapshots and sharing are unaffected; and echo
				// suppression (below) is inactive on a bare resize, so real
				// command output is never dropped.
				lineRedraw := isLineRedraw(chunk, stripped)

				// Echo suppression: buffer PTY output until all echoed
				// commands have arrived, then strip them and flush the
				// remainder. The PTY splits output at arbitrary byte
				// boundaries (e.g. "ech" | "o hello\nhello\n"), so we
				// cannot match per-chunk — we must accumulate first.
				//
				// Cmds is a queue: rapid command submission appends entries
				// so that all pending echoes are stripped in one pass.
				state := p.echoSuppress.Load()
				if state != nil && len(state.Cmds) > 0 {
					if time.Now().After(state.Deadline) {
						// Deadline expired — flush buffered data as-is.
						flushed := suppressBuf + stripped
						suppressBuf = ""
						p.echoSuppress.CompareAndSwap(state, nil)
						if flushed != "" {
							_ = pub.SendPaneShellOutput(id, applyPromptStrip(flushed))
						}
					} else {
						// Accumulate into local buffer.
						suppressBuf += stripped
						// Try to strip ALL pending command echoes from the buffer.
						// Work on a copy so we don't corrupt the buffer if not
						// all echoes have arrived yet.
						testBuf := suppressBuf
						allFound := true
						for _, cmd := range state.Cmds {
							echoWithNL := cmd + "\n"
							if idx := strings.Index(testBuf, echoWithNL); idx >= 0 {
								testBuf = testBuf[:idx] + testBuf[idx+len(echoWithNL):]
							} else {
								allFound = false
								break
							}
						}
						if allFound {
							// All echoes stripped — try to commit.
							// CAS may fail if executeShell appended a new command
							// between Load() and now; in that case we keep the
							// buffer and retry on the next chunk with the updated state.
							if p.echoSuppress.CompareAndSwap(state, nil) {
								suppressBuf = ""
								if testBuf != "" {
									_ = pub.SendPaneShellOutput(id, applyPromptStrip(testBuf))
								}
							}
						}
						// else: keep buffering, not all echoes received yet.
					}
				} else if stripped != "" && !lineRedraw {
					_ = pub.SendPaneShellOutput(id, applyPromptStrip(display))
				}
			}
		}
		// A PTY read deadline fires for either of its two users: the idle
		// raw-publish flush or the cursor-hidden exit debounce. A timeout is NOT
		// an error/EOF — handle whichever is due, then recompute the deadline so
		// the still-pending user keeps it (and a no-work timeout clears it,
		// returning to a normal blocking read).
		if err != nil && os.IsTimeout(err) {
			// Idle/trailing raw flush: the interactive program buffered its final
			// per-redraw bytes and went idle (claude redraws only on input). Flush
			// the tail so it becomes visible without waiting for the next chunk.
			if !rawPubFlushDeadline.IsZero() && len(rawPubBuf) > 0 {
				flushRawPub()
			}
			rawPubFlushDeadline = time.Time{}

			// Exit debounce: a timeout while debouncing means the inline program
			// quit and the shell is idle, so commit the exit now — the event-driven
			// debounce in the n>0 path would otherwise never run again until the
			// next keystroke, leaving the pane in raw mode showing the shell's PS1.
			if debouncing && time.Since(exitDebounceStart) >= interactiveExitDebounce {
				wasInteractive := p.rawMode || p.cursorHidden
				p.cursorHidden = false
				debouncing = false
				if wasInteractive && !(p.rawMode || p.cursorHidden) {
					// Flush coalesced raw bytes before the mode reset so the final
					// pre-exit frame reaches subscribers first.
					flushRawPub()
					_ = msg.SendShareModeChange(pub, id, false, 0, 0)
					if relayFlag.Load() {
						_ = nc.Publish(relayExitSubj, nil)
						_ = nc.Flush()
						relayFlag.Store(false)
					}
					_ = pub.Send(msg.T("pane", id, "status"),
						&msg.MsgPaneStatusUpdate{Status: "idle"})
				}
			}
			// Recompute the deadline: re-arm for whichever user is still pending,
			// or clear it (no work) so the loop returns to a blocking read.
			recomputeReadDeadline()
			continue
		}
		if err != nil {
			// Flush any coalesced raw share bytes so no final output is lost on
			// loop exit (byte order preserved).
			flushRawPub()
			// Flush any held chunk tail so a final line is not lost when the
			// shell exits with a read still pending (a lone CR or an
			// unterminated escape renders as nothing, so this is safe).
			if pendingTail != "" {
				if tail := applyPromptStrip(stripAnsiEscapes(pendingTail)); tail != "" {
					_ = pub.SendPaneShellOutput(id, tail)
				}
			}
			// Flush any remaining suppress buffer on EOF/error.
			if suppressBuf != "" {
				_ = pub.SendPaneShellOutput(id, applyPromptStrip(suppressBuf))
			}
			p.echoSuppress.Store(nil)
			if err != io.EOF {
				_ = pub.SendPaneShellOutput(id, fmt.Sprintf("shell read error: %v\n", err))
			}
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Relay activation / deactivation
// ---------------------------------------------------------------------------

// handleRelayActivate enters relay mode: rawReadLoop begins publishing raw PTY
// bytes to NATS for the TUI to write directly to stdout. The PTY is resized to
// the caller's terminal dimensions and a SIGWINCH is forced so the interactive
// program redraws at the new size.
func (p *PaneActor) handleRelayActivate(cols, rows int) {
	p.relayActive.Store(true)
	if p.ptyFile != nil && cols > 0 && rows > 0 {
		ws := pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
		// Force SIGWINCH by briefly shrinking then restoring, guaranteeing
		// the interactive program redraws even if sizes happen to match.
		shrunk := ws
		if shrunk.Rows > 1 {
			shrunk.Rows--
		}
		_ = pty.Setsize(p.ptyFile, &shrunk)
		_ = pty.Setsize(p.ptyFile, &ws)

		// Relay resizes the PTY directly to the caller's terminal dimensions, so
		// the interactive program repaints at THIS size. Propagate the same size to
		// the authoritative ptyRows/ptyCols, the local VT emulator, and any share
		// subscribers. Otherwise the program redraws at the relay size while the
		// mirror's VTerm (and the size forwarded at interactive-entry) stays at the
		// pre-relay box width — the wider output then wraps/scrambles on the mirror.
		// The source's own display reads the raw PTY relay stream directly, so only
		// the share mirror saw this mismatch.
		if uint16(rows) != p.ptyRows || uint16(cols) != p.ptyCols {
			p.ptyRows = uint16(rows)
			p.ptyCols = uint16(cols)
			if p.vtermEmu != nil {
				p.vtermEmu.Resize(rows, cols)
			}
			if p.rawMode || p.cursorHidden {
				_ = msg.SendShareModeChange(p.pub, p.id, true, rows, cols)
			}
		}
	}
}

// handleRelayDeactivate exits relay mode: rawReadLoop stops publishing to the
// relay NATS subject and resumes normal output path.
func (p *PaneActor) handleRelayDeactivate() {
	p.relayActive.Store(false)
}

// replayShareState re-publishes the pane's current interactive share state so a
// newly (re)joined share subscriber resumes rendering the live interactive app.
//
// Share output flows over ephemeral NATS pub/sub, so a subscriber that joins
// (or reconnects) after the app already entered interactive mode never received
// the original mode-change and raw-screen messages — it would otherwise display
// stale scrollback/history. This re-announces interactive mode and replays a
// full-screen repaint on demand, triggered by a subscriber-join notification
// via UpstreamShareActor. It is a no-op when the pane is not interactive.
//
// The mode change is sent before the repaint bytes (mirroring the initial
// interactive-entry ordering in rawReadLoop) so the subscriber recreates its
// VTerm at the correct dimensions before the screen content arrives.
func (p *PaneActor) replayShareState() {
	if p.vtermEmu == nil || !(p.rawMode || p.cursorHidden) {
		return // not currently interactive — nothing to resume
	}
	rows := int(p.ptyRows)
	cols := int(p.ptyCols)
	_ = msg.SendShareModeChange(p.pub, p.id, true, rows, cols)
	if repaint := p.vtermEmu.Repaint(); len(repaint) > 0 {
		_ = msg.SendRawOutput(p.pub, p.id, repaint)
	}
}

// ---------------------------------------------------------------------------
// PTY resize handling
// ---------------------------------------------------------------------------

// paneSizeClaim is one viewport's measurement of a pane.
type paneSizeClaim struct {
	rows, cols int
}

// tuiClaimPrefix marks a size claim owned by a terminal UI process on this
// machine. The pid after it is what makes the claim self-expiring: a TUI can
// die by SIGKILL with no chance to withdraw anything, so the pane checks
// liveness itself rather than trusting a departure message that may never come.
const tuiClaimPrefix = "tui:"

// anonymousSizeClaim keys a claim from a client that sent no id — an older
// front-end build. Folding them all onto one key keeps such a client behaving
// exactly as it did before claims existed (its latest size simply replaces its
// previous one) instead of accumulating a new constraint per message, which
// would ratchet the pane down to nothing.
const anonymousSizeClaim = "anonymous"

// claimPaneSize records a viewport's size for this pane and re-sizes the PTY to
// the smallest claim currently held.
func (p *PaneActor) claimPaneSize(clientID string, rows, cols int) {
	if rows <= 0 || cols <= 0 {
		return
	}
	if clientID == "" {
		clientID = anonymousSizeClaim
	}
	if p.sizeClaims == nil {
		p.sizeClaims = make(map[string]paneSizeClaim, 2)
	}
	p.sizeClaims[clientID] = paneSizeClaim{rows: rows, cols: cols}
	p.applyEffectivePaneSize()
}

// releasePaneSize withdraws a viewport's claim (the web hub calls this when a
// WebSocket closes) and re-sizes to whatever remains.
func (p *PaneActor) releasePaneSize(clientID string) {
	if clientID == "" {
		clientID = anonymousSizeClaim
	}
	if _, held := p.sizeClaims[clientID]; !held {
		return
	}
	delete(p.sizeClaims, clientID)
	p.applyEffectivePaneSize()
}

// applyEffectivePaneSize sizes the PTY to the smallest live claim.
//
// Smallest wins because the constraint is asymmetric: a viewport LARGER than
// the PTY simply has unused space around the grid, while a viewport SMALLER
// than the PTY has to truncate or wrap it — which turns a full-screen app's
// display into garbage. Sizing to the minimum is the only choice where every
// attached viewport can render what the PTY produces.
//
// With no live claims the PTY is left exactly as it is. That matters on the
// path where every viewport has gone: a detached daemon keeps its shells
// running at their last size, and re-sizing them to some invented default
// would reflow long-running interactive apps for nobody's benefit.
func (p *PaneActor) applyEffectivePaneSize() {
	p.pruneDeadSizeClaims()

	rows, cols := 0, 0
	for _, c := range p.sizeClaims {
		if rows == 0 || c.rows < rows {
			rows = c.rows
		}
		if cols == 0 || c.cols < cols {
			cols = c.cols
		}
	}
	if rows <= 0 || cols <= 0 {
		return // no live viewport — keep the PTY as it stands
	}
	p.handleResize(rows, cols)
}

// sizeClaimReapInterval rate-limits reapStaleSizeClaims. Two seconds is short
// enough that a pane recovers its full size almost immediately after the
// viewport that was constraining it dies, and long enough that the liveness
// syscalls stay invisible next to the snapshot poll that triggers them.
const sizeClaimReapInterval = 2 * time.Second

// reapStaleSizeClaims re-sizes the pane if a viewport that was constraining it
// has died. It runs off snapshot requests rather than a timer, rate-limited to
// sizeClaimReapInterval.
//
// This exists because a claim's owner can vanish without the pane hearing about
// it. Kill a terminal UI and its "tui:<pid>" claim is still on record; the pane
// stays clamped to that terminal's dimensions even though only a much larger
// desktop-app window is left. Nothing would correct it, either — the app sends
// a resize when ITS layout changes, and a terminal dying elsewhere does not
// change the app's layout.
//
// It is skipped below two claims because that is the only case where a stale
// claim changes the answer: with one claim there is nothing else to fall back
// to, and dropping it would resize the pane for no viewport at all.
func (p *PaneActor) reapStaleSizeClaims() {
	if len(p.sizeClaims) < 2 {
		return
	}
	now := time.Now()
	if now.Sub(p.lastSizeClaimReap) < sizeClaimReapInterval {
		return
	}
	p.lastSizeClaimReap = now

	before := len(p.sizeClaims)
	p.pruneDeadSizeClaims()
	if len(p.sizeClaims) != before {
		p.applyEffectivePaneSize()
	}
}

// pruneDeadSizeClaims drops claims whose owner is provably gone.
//
// Only "tui:<pid>" claims can be tested this way, and they are the ones that
// need it: a terminal UI attaches over loopback NATS, so its process is always
// on this machine, and it can be killed without warning. Checking the pid
// covers a clean exit and a crash with the same code path — no departure
// message to lose, no timeout to tune.
//
// Web clients cannot be tested (the daemon sees only a socket) and are released
// explicitly by the hub instead; if the hub itself dies, so has this daemon.
//
// This runs on every claim and release rather than on a timer, so a stale clamp
// left by a killed TUI is cleared the next time any other viewport reports its
// size — which is exactly when the wrong size would start to matter.
func (p *PaneActor) pruneDeadSizeClaims() {
	for id := range p.sizeClaims {
		pidStr, ok := strings.CutPrefix(id, tuiClaimPrefix)
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 || !session.ProcessAlive(pid) {
			delete(p.sizeClaims, id)
		}
	}
}

// handleResize updates the PTY and virtual terminal dimensions.
func (p *PaneActor) handleResize(rows, cols int) {
	if rows <= 0 || cols <= 0 {
		return
	}
	oldRows, oldCols := p.ptyRows, p.ptyCols
	if uint16(rows) == oldRows && uint16(cols) == oldCols {
		return
	}
	oldCursorNearBottom := false
	if p.vtermEmu != nil {
		oldCursorRow, _ := p.vtermEmu.CursorPos()
		oldCursorNearBottom = oldCursorRow >= int(oldRows)-1
	}
	p.ptyRows = uint16(rows)
	p.ptyCols = uint16(cols)
	if p.ptyFile != nil {
		_ = pty.Setsize(p.ptyFile, &pty.Winsize{Rows: p.ptyRows, Cols: p.ptyCols})
	}
	if p.vtermEmu != nil {
		p.vtermEmu.Resize(rows, cols)
		if rows > int(oldRows) && oldCursorNearBottom && !p.rawMode && !p.cursorHidden {
			p.seedVTermCursorToBottom("resize-grow")
		}
	}

	// Notify sharing subscribers about dimension change during interactive mode.
	if p.rawMode || p.cursorHidden {
		_ = msg.SendShareModeChange(p.pub, p.id, true, rows, cols)
	}

	// Always publish the new dimensions to the .resized topic so that local
	// subscribers (e.g. RemoteShareListenerActor) can track this pane's size.
	_ = p.pub.Send(msg.T("pane", p.id, "resized"), &msg.MsgPaneResized{
		PaneID: p.id,
		Rows:   rows,
		Cols:   cols,
	})
}
