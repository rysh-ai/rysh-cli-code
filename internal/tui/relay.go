// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package tui

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"golang.org/x/term"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ErrRelayEscape is returned by PTYRelay.Run() when the user presses Ctrl+O
// to escape back to the TUI prefix mode.
var ErrRelayEscape = errors.New("relay: ctrl+o escape")

// ErrRelayLayout is returned by PTYRelay.Run() when the user presses Ctrl+L
// to escape back to the TUI layout mode (e.g. to minimize/un-maximize the
// pane with `m`). Without this, an interactive program maximized via the
// relay could only be exited via Ctrl+O, leaving no way to minimize it.
var ErrRelayLayout = errors.New("relay: ctrl+l layout")

// ErrRelayModeSwitch is returned by PTYRelay.Run() when the user presses Esc
// twice — the universal "switch modes" chord — so the TUI can cycle the pane's
// input mode (shell → prompt → rysh → chat) without leaving the interactive
// session for good. Cycling back to shell re-enters the relay, the app having
// kept running in the PTY. Mirrors the in-pane double-Esc gesture for raw panes.
var ErrRelayModeSwitch = errors.New("relay: esc esc mode switch")

// PTYRelay implements tea.ExecCommand. When Bubble Tea calls Run(), the relay
// takes over stdin/stdout and proxies data between the real terminal and the
// PTY via NATS — the daemon's rawReadLoop publishes raw PTY bytes to a
// dedicated NATS subject, and the relay writes them directly to stdout.
// Input from stdin is published back to the PaneActor via NATS.
type PTYRelay struct {
	nc     *nats.Conn
	pub    *msgpkg.NATSPublisher
	paneID string
	stdout io.Writer
}

// NewPTYRelay creates a new NATS-based relay for the given pane.
func NewPTYRelay(nc *nats.Conn, pub *msgpkg.NATSPublisher, paneID string) *PTYRelay {
	return &PTYRelay{nc: nc, pub: pub, paneID: paneID}
}

// SetStdin is called by Bubble Tea before Run(). We use os.Stdin.Fd() directly
// for dup/raw-mode, so we don't need to store this.
func (r *PTYRelay) SetStdin(io.Reader) {}

// SetStdout is called by Bubble Tea before Run().
func (r *PTYRelay) SetStdout(out io.Writer) { r.stdout = out }

// SetStderr is called by Bubble Tea before Run().
func (r *PTYRelay) SetStderr(io.Writer) {}

// Run implements the blocking relay loop. It puts the real terminal in raw mode,
// enters alt screen, subscribes to NATS relay output, activates relay mode on
// the daemon, and proxies data until the interactive program exits or the user
// presses Ctrl+O.
func (r *PTYRelay) Run() error {
	// Put the real terminal in raw mode. Bubble Tea's ReleaseTerminal()
	// restores cooked mode before calling Run(), so we must re-enter raw.
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	// Enter alt screen on the REAL terminal. The interactive program (vim,
	// htop, etc.) already entered alt screen on the PTY side, but Bubble
	// Tea exited its own alt screen via ReleaseTerminal(). Without this,
	// the PTY output (cursor positioning, colors) writes to the primary
	// screen and looks garbled.
	_, _ = os.Stdout.Write([]byte("\x1b[?1049h\x1b[2J\x1b[H"))
	defer os.Stdout.Write([]byte("\x1b[?1049l"))

	// Stop the terminal from reporting mouse events for the duration of the
	// relay, and restore the TUI's own tracking on the way out.
	//
	// Bubble Tea's ReleaseTerminal already disables mouse before Run() is
	// called, but that is a single write on a link we do not control: over a
	// laggy or lossy connection (mosh, ssh on a bad uplink) the disable lands
	// late, and every report that slips through in the meantime is pumped
	// straight into the child's stdin by the loop below. The child never asked
	// for mouse input — the relay is entered for any full-screen app regardless
	// of its mouse mode — so a wheel tick surfaces as literal "<65;50;54M" text.
	// Repeating the disable here is idempotent and narrows that window; the
	// mouseReportFilter on the stdin pump catches whatever was already in
	// flight.
	//
	// The re-enable is not symmetry for its own sake: Bubble Tea's
	// RestoreTerminal (v1.3.4) restores the alt screen, bracketed paste and
	// focus reporting but NOT mouse tracking, so without this the TUI comes back
	// from a relay with click-to-focus and drag-to-copy dead.
	_, _ = os.Stdout.Write([]byte(mouseTrackingOff))
	defer os.Stdout.Write([]byte(mouseTrackingOn))

	// Get terminal size for relay activation (the daemon will resize the
	// PTY to match the real terminal).
	cols, rows, _ := term.GetSize(fd)

	// Subscribe to the relay data and exit NATS subjects BEFORE activating
	// relay mode, so we don't miss any data.
	dataSubj := msgpkg.T("pane", r.paneID, "relay", "data")
	exitSubj := msgpkg.T("pane", r.paneID, "relay", "exit")

	dataCh := make(chan *nats.Msg, 256)
	dataSub, err := r.nc.ChanSubscribe(dataSubj, dataCh)
	if err != nil {
		return err
	}
	defer func() { _ = dataSub.Unsubscribe() }()

	exitCh := make(chan *nats.Msg, 1)
	exitSub, err := r.nc.ChanSubscribe(exitSubj, exitCh)
	if err != nil {
		return err
	}
	defer func() { _ = exitSub.Unsubscribe() }()

	// Activate relay mode on the daemon. The PaneActor's rawReadLoop will
	// start publishing raw PTY bytes to the data subject, and the daemon
	// will resize the PTY to our terminal dimensions.
	paneInbox := msgpkg.T("pane", r.paneID, "inbox")
	_ = r.pub.Send(paneInbox, &msgpkg.MsgRelayActivate{
		PaneID: r.paneID,
		Cols:   cols,
		Rows:   rows,
	})

	// Deactivate relay mode when we exit (defers run LIFO, this runs before
	// NATS unsubscribes above).
	defer func() {
		_ = r.pub.Send(paneInbox, &msgpkg.MsgRelayDeactivate{PaneID: r.paneID})
	}()

	// Dup the stdin fd so we can close it to cleanly unblock the stdin
	// goroutine when the relay exits.
	dupFd, err := syscall.Dup(fd)
	if err != nil {
		return err
	}
	stdinDup := os.NewFile(uintptr(dupFd), "relay-stdin")

	// Set up SIGWINCH handler to forward terminal resizes to the PTY.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	var (
		doneOnce sync.Once
		doneCh   = make(chan struct{})
		doneErr  error
	)
	finish := func(e error) {
		doneOnce.Do(func() {
			doneErr = e
			close(doneCh)
		})
	}

	// Goroutine 1: stdin → PaneActor via NATS rawinput (scan for Ctrl+O / Ctrl+L
	// escape and the Esc Esc "switch modes" gesture).
	go func() {
		buf := make([]byte, 4096)
		rawinputSubj := msgpkg.T("pane", r.paneID, "rawinput")
		var lastEsc time.Time // time of the previous lone-Esc keypress (double-Esc gesture)
		var mouse mouseReportFilter
		for {
			n, readErr := stdinDup.Read(buf)
			if n > 0 {
				// Mouse reporting is off for the whole relay (see above), so any
				// report arriving here is stale and must not reach the child.
				data := mouse.filter(buf[:n])
				if len(data) == 0 {
					if readErr != nil {
						finish(readErr)
						return
					}
					continue
				}
				// A fast Esc double-tap can land both 0x1b bytes in ONE read (the
				// kernel coalesced the presses). That is the double-Esc "switch
				// modes" gesture arriving whole — escape to the TUI immediately so it
				// cycles the input mode. Without this the two-single-byte-reads path
				// below never triggers and the escapes just forward to the app.
				if isCoalescedDoubleEsc(data) {
					finish(ErrRelayModeSwitch)
					return
				}
				// A lone Esc keypress arrives as a single 0x1b byte; escape
				// SEQUENCES (arrows, function keys) arrive as multi-byte bursts
				// (ESC [ …). So two consecutive single-byte Esc reads within
				// rawEscWindow are the double-Esc "switch modes" gesture. Forward
				// the first Esc to the app (a single Esc must still reach it); on
				// the second, escape to the TUI so it cycles the input mode.
				if len(data) == 1 && data[0] == 0x1b {
					now := time.Now()
					if !lastEsc.IsZero() && now.Sub(lastEsc) <= rawEscWindow {
						finish(ErrRelayModeSwitch)
						return
					}
					lastEsc = now
					_ = r.pub.Send(rawinputSubj, &msgpkg.MsgRawKeyInput{
						PaneID: r.paneID,
						Data:   data,
					})
					if readErr != nil {
						finish(readErr)
						return
					}
					continue
				}
				lastEsc = time.Time{}
				escaped := false
				for i := 0; i < len(data); i++ {
					// Ctrl+O (0x0f) escapes to prefix mode; Ctrl+L (0x0c)
					// escapes to layout mode. Both are intercepted here so the
					// rysh multiplexer controls stay reachable from a maximized
					// interactive pane — mirroring the in-pane raw-mode handler
					// which also intercepts these keys.
					if data[i] == 0x0f || data[i] == 0x0c {
						if i > 0 {
							_ = r.pub.Send(rawinputSubj, &msgpkg.MsgRawKeyInput{
								PaneID: r.paneID,
								Data:   data[:i],
							})
						}
						if data[i] == 0x0c {
							finish(ErrRelayLayout)
						} else {
							finish(ErrRelayEscape)
						}
						escaped = true
						break
					}
				}
				if escaped {
					return
				}
				_ = r.pub.Send(rawinputSubj, &msgpkg.MsgRawKeyInput{
					PaneID: r.paneID,
					Data:   data,
				})
			}
			if readErr != nil {
				finish(readErr)
				return
			}
		}
	}()

	// Goroutine 2: NATS relay.data → stdout; relay.exit → finish.
	go func() {
		for {
			select {
			case natMsg := <-dataCh:
				if len(natMsg.Data) > 0 {
					_, _ = r.stdout.Write(natMsg.Data)
				}
			case <-exitCh:
				// Alt screen exited — drain remaining data then stop.
				for {
					select {
					case natMsg := <-dataCh:
						if len(natMsg.Data) > 0 {
							_, _ = r.stdout.Write(natMsg.Data)
						}
					default:
						finish(nil)
						return
					}
				}
			case <-doneCh:
				return
			}
		}
	}()

	// Goroutine 3: SIGWINCH → PTY resize via NATS.
	go func() {
		for {
			select {
			case <-sigCh:
				if w, h, sizeErr := term.GetSize(fd); sizeErr == nil {
					_ = r.pub.Send(paneInbox, &msgpkg.MsgPaneResize{
						Cols: w,
						Rows: h,
					})
				}
			case <-doneCh:
				return
			}
		}
	}()

	<-doneCh

	// Close the dup'd stdin to unblock goroutine 1.
	stdinDup.Close()

	// Give the stdin goroutine a moment to exit.
	time.Sleep(10 * time.Millisecond)

	return doneErr
}
