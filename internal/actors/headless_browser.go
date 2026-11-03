// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// HeadlessBrowserActor executes browser_action requests for a pane in a
// CLI-owned headless Chromium — a drop-in peer of the desktop app's embedded
// browser on the same NATS subjects (`pane.<id>.browser.request/response`).
// It exists so web automations run without rysh-cli-app attached (Phase 4):
// `##web headless on` / `##auto web run --headless <name>` spawn it as a
// child of the PaneActor; its Chromium binds a per-profile user-data-dir
// under the browser-instances root, so auth persists across headless runs
// (log in once with `##web headless login <profile>`).
//
// Concurrency: actions execute in a goroutine capturing only immutable
// values (the goroutine-safe *cdp.Browser, the publisher, subjects) per the
// actor rules; the mailbox stays responsive for Stop.
type HeadlessBrowserActor struct {
	paneID     string
	profile    string
	url        string
	profileDir string
	pub        *msg.NATSPublisher
	nc         *nats.Conn

	br      *bridge.NATSBridge
	browser *cdp.Browser

	// binPath and launchWaits override the Chromium binary and the DevTools
	// wait of each launch attempt; tests only ("" / nil → cdp.FindChromium /
	// headlessLaunchWaits).
	binPath     string
	launchWaits []time.Duration
}

// A cold Chromium start is usually under a second, but on a loaded machine
// (a whole test suite, several browsers launching at once) one launch in a
// few dozen takes 10s+, and now and then one never prints its DevTools line
// at all. A longer wait covers the slow start; one fresh relaunch covers the
// wedged one, which no wait would.
var headlessLaunchWaits = []time.Duration{30 * time.Second, 30 * time.Second}

// NewHeadlessBrowserActor creates the actor. profileDir is the Chromium
// user-data-dir (…/browser-instances/<profile>/headless).
func NewHeadlessBrowserActor(paneID, profile, url, profileDir string, pub *msg.NATSPublisher, nc *nats.Conn) *HeadlessBrowserActor {
	return &HeadlessBrowserActor{
		paneID:     paneID,
		profile:    profile,
		url:        url,
		profileDir: profileDir,
		pub:        pub,
		nc:         nc,
	}
}

// Receive implements actor.Actor.
func (h *HeadlessBrowserActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		b, err := h.launch()
		if err != nil {
			slog.Error("headless: launch failed", "pane", h.paneID, "profile", h.profile, "err", err)
			_ = h.pub.SendPaneRyshOutput(h.paneID,
				fmt.Sprintf("[web] headless browser failed to start: %v\n", err))
			ctx.Stop(ctx.Self())
			return
		}
		h.browser = b

		h.br = bridge.New(h.nc, ctx.Self(), ctx.ActorSystem(), h.pub.Codecs())
		if err := h.br.AddSubject(msg.T("pane", h.paneID, "browser", "request")); err != nil {
			slog.Error("headless: subscribe browser.request", "pane", h.paneID, "err", err)
		}
		slog.Info("headless: browser running", "pane", h.paneID, "profile", h.profile, "dir", h.profileDir)
		_ = h.pub.SendPaneRyshOutput(h.paneID,
			fmt.Sprintf("[web] headless browser running (profile %q, auth dir %s)\n", h.profile, h.profileDir))

	case *actor.Stopping:
		if h.br != nil {
			h.br.Stop()
			h.br = nil
		}
		if h.browser != nil {
			h.browser.Close()
			h.browser = nil
		}
		slog.Info("headless: stopped", "pane", h.paneID)

	case *msg.MsgBrowserActionRequest:
		if h.browser == nil {
			h.reply(m.RequestID, nil, "", fmt.Errorf("headless browser not running"))
			return
		}
		// Execute off the mailbox: actions can take seconds (navigation,
		// waits). Captures only goroutine-safe/immutable values.
		browser, reqID, action, params := h.browser, m.RequestID, m.Action, m.Params
		go func() {
			result, screenshot, err := browser.Do(action, params)
			h.reply(reqID, result, screenshot, err)
		}()
	}
}

// launch starts the headless Chromium, relaunching once if the first attempt
// does not come up.
func (h *HeadlessBrowserActor) launch() (*cdp.Browser, error) {
	waits := h.launchWaits
	if len(waits) == 0 {
		waits = headlessLaunchWaits
	}
	var err error
	for i, wait := range waits {
		var b *cdp.Browser
		b, err = cdp.Launch(context.Background(), cdp.LaunchOptions{
			BinPath:     h.binPath,
			UserDataDir: h.profileDir,
			Headless:    true,
			URL:         h.url,
			WaitTimeout: wait,
		})
		if err == nil {
			return b, nil
		}
		if i < len(waits)-1 {
			slog.Warn("headless: launch failed, relaunching", "pane", h.paneID, "profile", h.profile, "attempt", i+1, "err", err)
		}
	}
	return nil, err
}

// reply publishes the MsgBrowserActionResponse the browser_action tool is
// waiting for.
func (h *HeadlessBrowserActor) reply(requestID string, result []byte, screenshot string, err error) {
	resp := &msg.MsgBrowserActionResponse{
		RequestID:  requestID,
		Success:    err == nil,
		Result:     result,
		Screenshot: screenshot,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	_ = h.pub.Send(msg.T("pane", h.paneID, "browser", "response"), resp)
}
