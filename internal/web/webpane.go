// Server-side embedded web panes for browser mode (web_electron_roadmap W12).
//
// In the Electron app a web pane is a native WebContentsView overlaid on the
// pane. A plain browser tab cannot embed arbitrary third-party pages
// (X-Frame-Options / CSP) nor attach a debugger — so in web mode the server
// drives a HEADLESS Chromium (internal/cdp, the same engine behind rysh's
// browser automation) and streams JPEG frames + url/title to the pane over
// the existing /ws protocol:
//
//	client → server: webpane_open {pane_id,url,profile} · webpane_navigate
//	                 {pane_id,url} · webpane_back/forward/reload/close {pane_id}
//	server → client: webpane_frame {pane_id,url,title,screenshot}   (~1 fps)
//	                 webpane_error {pane_id,error}                  (fail visible)
//
// Availability is advertised in /api/env capabilities.web_pane (needs a
// workspace root for the profile dir and a Chromium binary). When it is
// false, the renderer keeps its explicit "requires the desktop app" note —
// the capability degrades visibly, never silently.
//
// Profiles: each rysh web profile maps to its own persistent user-data-dir
// under <workspace>/.rysh/browser-instances/webpane/<profile> — deliberately
// NOT the interactive-login profile dir (Chromium locks user-data-dirs, and a
// headed login browser may hold that lock).
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/browserinstance"
	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
)

// webPaneFrameInterval is how often a live server-side pane pushes a frame
// while at least one stream client is connected.
const webPaneFrameInterval = 1200 * time.Millisecond

// webPaneSession is one server-driven browser bound to a pane.
type webPaneSession struct {
	paneID  string
	profile string

	mu      sync.Mutex
	browser *cdp.Browser
	cancel  context.CancelFunc
}

// webPaneAvailable reports whether server-side web panes can run here.
func (s *Server) webPaneAvailable() bool {
	wsPath, _ := s.workspaceInfo()
	return wsPath != "" && cdp.FindChromium() != ""
}

// webPaneProfileDir is the persistent Chromium user-data-dir for a web-mode
// pane profile.
func webPaneProfileDir(workDir, profile string) string {
	if profile == "" {
		profile = "default"
	}
	return filepath.Join(browserinstance.RootDir(workDir), "webpane", browserinstance.SanitizeProfile(profile))
}

// handleWebPaneCommand dispatches the webpane_* ws commands. All work that
// can block (launching Chromium, navigation, screenshots) runs off the
// readPump goroutine; results and errors are pushed as frames/error events.
func (s *Server) handleWebPaneCommand(action string, data json.RawMessage) {
	var cmd struct {
		PaneID  string `json:"pane_id"`
		URL     string `json:"url"`
		Profile string `json:"profile"`
	}
	if json.Unmarshal(data, &cmd) != nil || cmd.PaneID == "" {
		return
	}

	switch action {
	case "webpane_open":
		go s.webPaneOpen(cmd.PaneID, cmd.URL, cmd.Profile)
	case "webpane_close":
		s.webPaneClose(cmd.PaneID)
	case "webpane_navigate":
		go s.webPaneDo(cmd.PaneID, "navigate", cmd.URL)
	case "webpane_back":
		go s.webPaneDo(cmd.PaneID, "back", "")
	case "webpane_forward":
		go s.webPaneDo(cmd.PaneID, "forward", "")
	case "webpane_reload":
		go s.webPaneDo(cmd.PaneID, "reload", "")
	}
}

// webPaneOpen creates (or reuses) the server-side browser for a pane and
// starts its frame loop. Idempotent per pane+profile: reopening with the same
// profile keeps the live page (mirrors the Electron manager's reattach).
func (s *Server) webPaneOpen(paneID, url, profile string) {
	wsPath, _ := s.workspaceInfo()
	if wsPath == "" || cdp.FindChromium() == "" {
		s.pushWebPaneError(paneID, "server-side web panes unavailable: this rysh server has no workspace/Chromium — use the desktop app for embedded pages")
		return
	}

	s.webPaneMu.Lock()
	if s.webPanes == nil {
		s.webPanes = make(map[string]*webPaneSession)
	}
	if sess, ok := s.webPanes[paneID]; ok && sess.profile == profile {
		s.webPaneMu.Unlock()
		if url != "" {
			s.webPaneDo(paneID, "navigate", url)
		}
		return
	} else if ok {
		// Profile changed: tear down and relaunch against the new partition.
		delete(s.webPanes, paneID)
		s.webPaneMu.Unlock()
		sess.close()
		s.webPaneMu.Lock()
	}
	sess := &webPaneSession{paneID: paneID, profile: profile}
	s.webPanes[paneID] = sess
	s.webPaneMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	launchCtx, launchDone := context.WithTimeout(ctx, 45*time.Second)
	b, err := cdp.Launch(launchCtx, cdp.LaunchOptions{
		UserDataDir: webPaneProfileDir(wsPath, profile),
		Headless:    true,
		URL:         url,
	})
	launchDone()
	if err != nil {
		cancel()
		s.webPaneMu.Lock()
		if s.webPanes[paneID] == sess {
			delete(s.webPanes, paneID)
		}
		s.webPaneMu.Unlock()
		s.pushWebPaneError(paneID, fmt.Sprintf("server-side browser failed to start: %v", err))
		return
	}

	sess.mu.Lock()
	sess.browser = b
	sess.cancel = cancel
	sess.mu.Unlock()

	s.pushWebPaneFrame(sess)
	go s.webPaneFrameLoop(ctx, sess)
}

// webPaneDo runs a navigation action on a pane's browser and pushes a fresh
// frame (or a visible error).
func (s *Server) webPaneDo(paneID, action, url string) {
	s.webPaneMu.Lock()
	sess := s.webPanes[paneID]
	s.webPaneMu.Unlock()
	if sess == nil {
		s.pushWebPaneError(paneID, "no server-side browser for this pane — reopen the web pane")
		return
	}
	sess.mu.Lock()
	b := sess.browser
	sess.mu.Unlock()
	if b == nil {
		return
	}
	var params json.RawMessage
	if action == "navigate" {
		if url == "" {
			return
		}
		p, _ := json.Marshal(map[string]string{"url": url})
		params = p
	}
	if _, _, err := b.Do(action, params); err != nil {
		s.pushWebPaneError(paneID, fmt.Sprintf("%s failed: %v", action, err))
		return
	}
	s.pushWebPaneFrame(sess)
}

// webPaneClose tears down a pane's server-side browser.
func (s *Server) webPaneClose(paneID string) {
	s.webPaneMu.Lock()
	sess := s.webPanes[paneID]
	delete(s.webPanes, paneID)
	s.webPaneMu.Unlock()
	if sess != nil {
		sess.close()
	}
}

// closeAllWebPanes tears down every server-side browser (server Stop).
func (s *Server) closeAllWebPanes() {
	s.webPaneMu.Lock()
	sessions := s.webPanes
	s.webPanes = nil
	s.webPaneMu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}
}

func (w *webPaneSession) close() {
	w.mu.Lock()
	b := w.browser
	cancel := w.cancel
	w.browser = nil
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if b != nil {
		b.Close()
	}
}

// webPaneFrameLoop pushes a frame roughly once a second while any stream
// client is connected, until the session closes.
func (s *Server) webPaneFrameLoop(ctx context.Context, sess *webPaneSession) {
	ticker := time.NewTicker(webPaneFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sess.mu.Lock()
			gone := sess.browser == nil
			sess.mu.Unlock()
			if gone {
				return
			}
			if !s.hub.hasStream() {
				continue
			}
			s.pushWebPaneFrame(sess)
		}
	}
}

// pushWebPaneFrame captures url/title + a JPEG screenshot and broadcasts a
// webpane_frame to stream clients.
func (s *Server) pushWebPaneFrame(sess *webPaneSession) {
	sess.mu.Lock()
	b := sess.browser
	sess.mu.Unlock()
	if b == nil {
		return
	}

	var url, title string
	if raw, _, err := b.Do("execute_js", json.RawMessage(`{"code":"JSON.stringify({url:location.href,title:document.title})"}`)); err == nil {
		var wrap struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(raw, &wrap) == nil && wrap.Result != "" {
			var pg struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			}
			if json.Unmarshal([]byte(wrap.Result), &pg) == nil {
				url, title = pg.URL, pg.Title
			}
		}
	}

	_, shot, err := b.Do("screenshot", json.RawMessage(`{"format":"jpeg","quality":55}`))
	if err != nil {
		return
	}

	data, err := json.Marshal(map[string]interface{}{
		"type": "webpane_frame",
		"data": map[string]interface{}{
			"pane_id":    sess.paneID,
			"url":        url,
			"title":      title,
			"screenshot": shot, // base64 JPEG
		},
	})
	if err != nil {
		return
	}
	s.hub.sendWhere(data, func(c *wsClient) bool { return c.streamContent })
}

// pushWebPaneError surfaces a web-pane failure in the UI (fail visible).
func (s *Server) pushWebPaneError(paneID, errMsg string) {
	data, err := json.Marshal(map[string]interface{}{
		"type": "webpane_error",
		"data": map[string]interface{}{"pane_id": paneID, "error": errMsg},
	})
	if err != nil {
		return
	}
	s.hub.sendAll(data)
}
