// Package cdp is a minimal Chrome DevTools Protocol client used by rysh's
// headless web mode: it launches a Chromium with --remote-debugging-port,
// attaches over the browser-level WebSocket (flat session mode), and executes
// browser_action requests (see actions.go) without needing the desktop app.
//
// Deliberately dependency-free beyond gorilla/websocket (already a rysh-cli
// dependency): rysh needs a dozen CDP methods, not a full protocol binding.
package cdp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FindChromium locates a Chromium/Chrome binary: RYSH_CHROMIUM_PATH first,
// then PATH candidates, then platform install locations. Returns "" when
// nothing is found.
func FindChromium() string {
	if p := strings.TrimSpace(os.Getenv("RYSH_CHROMIUM_PATH")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome", "brave-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "linux":
		candidates = []string{
			"/usr/bin/chromium", "/usr/bin/chromium-browser",
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
			"/snap/bin/chromium",
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// LaunchOptions configures a browser launch.
type LaunchOptions struct {
	BinPath     string // Chromium binary ("" → FindChromium)
	UserDataDir string // profile directory (required — this is where auth persists)
	Headless    bool   // --headless=new when true; headed for interactive login
	URL         string // initial URL ("" → about:blank)
	// WaitTimeout bounds the wait for Chromium's "DevTools listening on"
	// line (0 → 20s). Cold starts on loaded machines — CI runners under
	// -race, several browsers launching at once — can exceed the default;
	// callers that can afford to wait longer pass a larger bound. The
	// caller's ctx still cancels earlier if it expires first.
	WaitTimeout time.Duration
}

// devtoolsWaitTimeout resolves the effective wait bound for the DevTools
// endpoint line. Split out for testing.
func devtoolsWaitTimeout(o LaunchOptions) time.Duration {
	if o.WaitTimeout > 0 {
		return o.WaitTimeout
	}
	return 20 * time.Second
}

// launchArgs builds the Chromium command line. Split out for testing.
func launchArgs(o LaunchOptions) []string {
	args := []string{
		"--remote-debugging-port=0", // Chromium picks a free port; parsed from stderr
		"--user-data-dir=" + o.UserDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--disable-features=Translate,MediaRouter",
		"--window-size=1280,900",
		"--remote-allow-origins=*",
	}
	if o.Headless {
		args = append(args, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--mute-audio")
		// Container/CI survival flags. Headless-only: a headed launch is a real
		// desktop session where both problems are absent, and where silently
		// dropping the sandbox would be indefensible. See sandbox.go.
		sbxArgs, _ := sandboxArgs(probeSandboxEnv())
		args = append(args, sbxArgs...)
	}
	url := o.URL
	if url == "" {
		url = "about:blank"
	}
	return append(args, url)
}

// Launch starts Chromium and connects to its DevTools WebSocket. The caller
// owns the returned Browser and must Close it (which kills the process).
func Launch(ctx context.Context, o LaunchOptions) (*Browser, error) {
	bin := o.BinPath
	if bin == "" {
		bin = FindChromium()
	}
	if bin == "" {
		return nil, fmt.Errorf("no Chromium/Chrome binary found — install one or set RYSH_CHROMIUM_PATH")
	}
	if o.UserDataDir == "" {
		return nil, fmt.Errorf("UserDataDir is required (browser auth persists there)")
	}
	if err := os.MkdirAll(o.UserDataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create user data dir: %w", err)
	}
	if o.Headless {
		// Logged here rather than in launchArgs so it fires once per launch,
		// not once per call of a pure helper.
		if _, reason := sandboxArgs(probeSandboxEnv()); reason != "" {
			warnSandboxDisabled(reason)
		}
	}

	// SingletonLock race guard: a headless relaunch on the same profile
	// (##web headless on / ##auto web run --headless while an executor is
	// already up) races the previous Chromium's asynchronous shutdown — and
	// Chromium refuses to start on a locked user-data-dir. Wait for the
	// previous owner (identified by our pidfile) to exit, reaping it if its
	// shutdown wedges past the grace window. Also covers launching onto a
	// profile still held by an orphan from a crashed daemon.
	if o.Headless {
		waitProfileFree(ctx, o.UserDataDir, 8*time.Second)
	}

	cmd := exec.Command(bin, launchArgs(o)...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", filepath.Base(bin), err)
	}

	// Chromium prints "DevTools listening on ws://127.0.0.1:PORT/devtools/browser/UUID"
	// to stderr once the debugging endpoint is up.
	wsCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, "DevTools listening on "); idx >= 0 {
				select {
				case wsCh <- strings.TrimSpace(line[idx+len("DevTools listening on "):]):
				default:
				}
				break
			}
		}
		// Drain the remainder so Chromium never blocks on a full stderr pipe.
		for scanner.Scan() {
		}
	}()

	var wsURL string
	select {
	case wsURL = <-wsCh:
	case <-time.After(devtoolsWaitTimeout(o)):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("timed out waiting for Chromium DevTools endpoint (%v)", devtoolsWaitTimeout(o))
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	}

	b, err := connect(ctx, wsURL, cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	if err := b.attachFirstPage(); err != nil {
		b.Close()
		return nil, err
	}
	// Headless launches record their PID so SweepOrphanedHeadless can reap a
	// Chromium that outlives a force-killed daemon. Headed (login) launches
	// deliberately never write one — a sweep must not kill the user's window.
	if o.Headless {
		b.pidFileDir = o.UserDataDir
		writeHeadlessPidFile(o.UserDataDir, cmd.Process.Pid)
	}
	return b, nil
}

// LaunchInteractive starts a HEADED Chromium on the profile directory without
// attaching CDP — the interactive-login helper (`##web headless login`). The
// process is detached; the user logs in and closes the window, and the auth
// lands in the profile dir for subsequent headless runs.
func LaunchInteractive(userDataDir, url string) (int, error) {
	bin := FindChromium()
	if bin == "" {
		return 0, fmt.Errorf("no Chromium/Chrome binary found — install one or set RYSH_CHROMIUM_PATH")
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return 0, err
	}
	if url == "" {
		url = "about:blank"
	}
	cmd := exec.Command(bin,
		"--user-data-dir="+userDataDir,
		"--no-first-run", "--no-default-browser-check", "--disable-sync",
		"--window-size=1280,900", url)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap; user closes the window when done
	return pid, nil
}
