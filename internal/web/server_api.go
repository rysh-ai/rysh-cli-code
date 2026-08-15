// SPDX-License-Identifier: Apache-2.0

// Parity API surface (web_electron_roadmap Phase 3, tasks W7–W10).
//
// The Electron desktop app gets a handful of conveniences from its main
// process (`window.electronAPI`): env/platform info, workspace reads, voice
// transcription and shell tab-completion. In web mode there is no Electron
// main process — but the daemon this server runs inside owns the same data
// and the same Go engines, so each capability is served here instead:
//
//	W9  GET  /api/env             → is_web flag + platform + session + capability map
//	W8  GET  /api/workspaces      → current workspace + on-disk session registry
//	W10 GET  /api/voice/config    → public voice config (never the API key)
//	W10 POST /api/voice/transcribe→ audio body → transcript (internal/voice)
//	W7  ws   completion_get       → completion_result (internal/tui engine),
//	                                replied ONLY to the requesting client
//
// All /api/* routes sit behind the same access-token middleware as the rest
// of the server (auth.go); nothing here widens the surface. Everything is
// read-only except transcription, which only calls the configured provider.
package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/tui"
	"github.com/rysh-ai/rysh-cli-code/internal/voice"
)

// VoiceSettings is the web server's view of the daemon's voice configuration
// ([voice] + [voice_control] in rysh.config.yaml). Injected by the workspace
// actor via SetVoice before Start. The APIKey never leaves the server.
type VoiceSettings struct {
	Enabled  bool
	Provider string // "deepgram" (default) | "whisper"
	APIKey   string
	Hotkey   string // e.g. "ctrl+r"
	Language string // optional BCP-47 hint
}

// WorkspaceEntry is one workspace visible to the web UI: the current one, or
// a recent one recorded in the on-disk session registry.
type WorkspaceEntry struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	LastOpened  string `json:"last_opened,omitempty"`
	LastSession string `json:"last_session,omitempty"`
}

// SetVoice injects the daemon's voice configuration. Call before Start.
func (s *Server) SetVoice(v VoiceSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(v.Provider) == "" {
		v.Provider = "deepgram"
	}
	if strings.TrimSpace(v.Hotkey) == "" {
		v.Hotkey = "ctrl+r"
	}
	s.voice = v
}

// SetBashCompletion enables delegation to bash's programmable completion
// specs for argument-position tab-completion (config [ui]
// shell_bash_completion). Built-in command/path completion is always on.
func (s *Server) SetBashCompletion(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bashCompletion = enabled
}

// SetWorkspaceInfo records the workspace this session runs in (working
// directory + display name), served by /api/env and /api/workspaces and used
// as the root for server-side web-pane browser profiles (W12).
func (s *Server) SetWorkspaceInfo(path, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wsPath = strings.TrimSpace(path)
	s.wsName = strings.TrimSpace(name)
	if s.wsName == "" && s.wsPath != "" {
		s.wsName = filepath.Base(s.wsPath)
	}
}

// SetWorkspaceLister injects the recent-workspace source (the on-disk session
// registry, read by the actors package — this package cannot import it).
// nil ⇒ /api/workspaces returns only the current workspace.
func (s *Server) SetWorkspaceLister(fn func() []WorkspaceEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceLister = fn
}

// voiceSettings returns a copy of the injected voice settings, with defaults
// normalized even when SetVoice was never called (no config ⇒ voice disabled
// but the reported provider/hotkey still make sense).
func (s *Server) voiceSettings() VoiceSettings {
	s.mu.Lock()
	v := s.voice
	s.mu.Unlock()
	if strings.TrimSpace(v.Provider) == "" {
		v.Provider = "deepgram"
	}
	if strings.TrimSpace(v.Hotkey) == "" {
		v.Hotkey = "ctrl+r"
	}
	return v
}

// workspaceInfo returns the injected workspace path + name.
func (s *Server) workspaceInfo() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wsPath, s.wsName
}

// voiceUsable reports whether transcription can actually run: an API key is
// configured and voice is not explicitly disabled. Mirrors the desktop app's
// publicVoiceConfig (a configured key implies enabled).
func (v VoiceSettings) usable() bool {
	return strings.TrimSpace(v.APIKey) != ""
}

// registerParityAPI mounts the read-only parity endpoints (W8/W9/W10).
func (s *Server) registerParityAPI(r gin.IRouter) {
	api := r.Group("/api")
	api.GET("/env", s.handleEnv)
	api.GET("/workspaces", s.handleWorkspaces)
	api.GET("/voice/config", s.handleVoiceConfig)
	api.POST("/voice/transcribe", s.handleVoiceTranscribe)
}

// handleEnv answers GET /api/env — the renderer's authoritative "am I in web
// mode, and what can this server do" signal (W9). Everything the desktop app
// learns from getPlatform/getSessionName/voice.getConfig at startup arrives
// here in one fetch, plus a capability map so the UI shows real affordances
// instead of feature-sniffing (fail visible, not silent).
func (s *Server) handleEnv(c *gin.Context) {
	wsPath, wsName := s.workspaceInfo()
	v := s.voiceSettings()
	c.JSON(http.StatusOK, gin.H{
		"is_web":       true,
		"platform":     runtime.GOOS,
		"session_name": s.sessionName,
		"control":      s.ControlEnabled(),
		"workspace":    gin.H{"path": wsPath, "name": wsName},
		"voice":        publicVoice(v),
		"capabilities": gin.H{
			"completion": true,
			"workspaces": true,
			"voice":      v.usable(),
			// Server-side embedded web panes (W12): need a workspace root for
			// the browser profile and a Chromium binary on this machine.
			"web_pane": s.webPaneAvailable(),
			// Genuinely-native Electron controls — never available from a page.
			"restart_daemon": false,
			"native_open":    false,
		},
	})
}

// publicVoice is the key-free voice config shape shared by /api/env and
// /api/voice/config (same fields the Electron bridge's voice.getConfig returns).
func publicVoice(v VoiceSettings) gin.H {
	return gin.H{
		"enabled":  v.usable(),
		"provider": v.Provider,
		"hotkey":   v.Hotkey,
		"language": v.Language,
	}
}

// handleWorkspaces answers GET /api/workspaces (W8): the current workspace
// plus the recent ones from the session registry.
func (s *Server) handleWorkspaces(c *gin.Context) {
	wsPath, wsName := s.workspaceInfo()
	s.mu.Lock()
	lister := s.workspaceLister
	s.mu.Unlock()

	var current *WorkspaceEntry
	if wsPath != "" {
		current = &WorkspaceEntry{Path: wsPath, Name: wsName}
	}
	recent := []WorkspaceEntry{}
	if lister != nil {
		if list := lister(); list != nil {
			recent = list
		}
	}
	c.JSON(http.StatusOK, gin.H{"current": current, "recent": recent})
}

// handleVoiceConfig answers GET /api/voice/config (W10).
func (s *Server) handleVoiceConfig(c *gin.Context) {
	c.JSON(http.StatusOK, publicVoice(s.voiceSettings()))
}

// maxTranscribeBody bounds an uploaded recording (browser MediaRecorder audio;
// 2 minutes of opus is well under this).
const maxTranscribeBody = 24 << 20 // 24 MiB

// handleVoiceTranscribe answers POST /api/voice/transcribe (W10): raw audio
// bytes in the body (Content-Type = the recording's MIME type) → transcript.
// The provider call runs server-side via internal/voice — the exact engine the
// TUI's voice feature uses — so the API key never reaches the page. The reply
// mirrors the Electron bridge: 200 + {"transcript": ...} or {"error": ...}.
func (s *Server) handleVoiceTranscribe(c *gin.Context) {
	v := s.voiceSettings()
	if !v.usable() {
		c.JSON(http.StatusOK, gin.H{
			"error": "voice: no API key configured (set [voice_control].api_key in rysh.config.yaml)",
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxTranscribeBody+1))
	if err != nil || len(body) == 0 {
		c.JSON(http.StatusOK, gin.H{"error": "voice: empty or unreadable audio body"})
		return
	}
	if len(body) > maxTranscribeBody {
		c.JSON(http.StatusOK, gin.H{"error": "voice: recording too large"})
		return
	}

	// The transcriber takes a file path (and derives the provider content type
	// from its extension), so stage the upload in a temp file.
	ext := audioExtForMime(c.ContentType())
	f, err := os.CreateTemp("", "rysh-voice-*"+ext)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "voice: " + err.Error()})
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		c.JSON(http.StatusOK, gin.H{"error": "voice: " + err.Error()})
		return
	}
	f.Close()

	transcribe := s.transcribeFn
	if transcribe == nil {
		transcribe = realTranscribe
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	text, err := transcribe(ctx, v, tmp)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "voice: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"transcript": text})
}

// realTranscribe is the production transcription path (overridden in tests
// via Server.transcribeFn).
func realTranscribe(ctx context.Context, v VoiceSettings, audioPath string) (string, error) {
	tr, err := voice.NewTranscriber(v.Provider, v.APIKey, v.Language)
	if err != nil {
		return "", err
	}
	return tr.Transcribe(ctx, audioPath)
}

// audioExtForMime maps a browser recording MIME type to a file extension the
// transcriber (and the Whisper multipart filename) understands.
func audioExtForMime(mime string) string {
	mime = strings.ToLower(mime)
	switch {
	case strings.Contains(mime, "ogg"):
		return ".ogg"
	case strings.Contains(mime, "wav"):
		return ".wav"
	case strings.Contains(mime, "mp4"), strings.Contains(mime, "m4a"), strings.Contains(mime, "aac"):
		return ".mp4"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return ".mp3"
	default:
		return ".webm" // MediaRecorder's usual default
	}
}

// ---------------------------------------------------------------------------
// W7 — shell tab-completion over the existing /ws protocol (request/reply)
// ---------------------------------------------------------------------------

// handleCompletionGet serves the ws "completion_get" command: run the shared
// TUI completion engine for the token under the cursor and reply — to the
// REQUESTING client only — with a "completion_result" carrying the request_id
// for correlation. Filesystem/bash work is bounded (~400ms bash timeout) but
// still runs off the readPump goroutine so a slow disk cannot stall the
// client's inbound command stream.
func (s *Server) handleCompletionGet(c *wsClient, data json.RawMessage) {
	var cmd struct {
		RequestID    string `json:"request_id"`
		PaneID       string `json:"pane_id"`
		ShellPID     int    `json:"shell_pid"`
		Token        string `json:"token"`
		Line         string `json:"line"`
		Cwd          string `json:"cwd"`
		IsFirstToken bool   `json:"is_first_token"`
	}
	if json.Unmarshal(data, &cmd) != nil || cmd.RequestID == "" {
		return
	}
	s.mu.Lock()
	useBash := s.bashCompletion
	s.mu.Unlock()

	go func() {
		cands := tui.ShellCompletions(tui.ShellCompletionRequest{
			Token:             cmd.Token,
			Line:              cmd.Line,
			Cwd:               cmd.Cwd,
			ShellPID:          cmd.ShellPID,
			IsFirstToken:      cmd.IsFirstToken,
			UseBashCompletion: useBash,
		})
		if cands == nil {
			cands = []tui.ShellCompletionCandidate{}
		}
		reply, err := json.Marshal(map[string]interface{}{
			"type": "completion_result",
			"data": map[string]interface{}{
				"request_id": cmd.RequestID,
				"pane_id":    cmd.PaneID,
				"candidates": cands,
			},
		})
		if err != nil {
			return
		}
		// Targeted reply: only the asking client cares (and a broadcast would
		// leak one user's input line to every connected viewer).
		select {
		case c.send <- reply:
		default: // client too slow / gone — drop, the UI just gets no menu
		}
	}()
}

// handlePaneResize records how much room THIS connection's renderer gives a
// pane, and tells the pane about it.
//
// It is a size CLAIM, not a command. One pane can be on screen in several
// viewports at once — this window, another app window, a terminal UI attached
// to the same daemon — and each measures it differently. The pane sizes its
// single PTY to the smallest claim so the grid fits in all of them; without the
// client id these viewports would overwrite each other last-writer-wins and an
// interactive app would reflow on every frame. See msg.MsgPaneResize.
//
// Without any resize at all the PTY stays at its 80x24 default, so interactive
// apps (vim, claude) fill only ~24 rows regardless of the pane's real height.
func (s *Server) handlePaneResize(c *wsClient, data json.RawMessage) {
	var cmd struct {
		PaneID string `json:"pane_id"`
		Rows   int    `json:"rows"`
		Cols   int    `json:"cols"`
	}
	if json.Unmarshal(data, &cmd) != nil || cmd.PaneID == "" || cmd.Rows <= 0 || cmd.Cols <= 0 {
		return
	}
	clientID := ""
	if c != nil {
		clientID = c.sizeClientID
		if c.sizedPanes != nil {
			// Remember it so the claim can be withdrawn when this socket closes.
			c.sizedPanes[cmd.PaneID] = true
		}
	}
	_ = s.pub.Send(msg.T("pane", cmd.PaneID, "inbox"), &msg.MsgPaneResize{
		Rows:     cmd.Rows,
		Cols:     cmd.Cols,
		ClientID: clientID,
	})
}

// releasePaneSizes withdraws every pane size claim a closing connection made,
// so the panes it was sizing re-expand to whatever viewports remain.
//
// This is load-bearing in a way the TUI's equivalent is not: a pane prunes a
// terminal UI's claim by testing whether its pid is still alive, but a web
// client is just a socket the daemon can no longer see. If this does not run,
// a closed app window keeps every pane it ever sized clamped to that window's
// dimensions for the rest of the daemon's life.
func (s *Server) releasePaneSizes(c *wsClient) {
	if c == nil || len(c.sizedPanes) == 0 {
		return
	}
	for paneID := range c.sizedPanes {
		_ = s.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneReleaseSize{ClientID: c.sizeClientID})
	}
	c.sizedPanes = nil
}

// handleClientCommand routes ws commands that need the ORIGINATING client —
// request/reply exchanges (completion W7, board_get in board.go) and per-client
// web-pane streams (W12) — and falls back to the broadcast-style handleCommand
// for the rest.
func (s *Server) handleClientCommand(c *wsClient, action string, data json.RawMessage) {
	switch action {
	case "completion_get":
		s.handleCompletionGet(c, data)
	case "pane_resize":
		s.handlePaneResize(c, data)
	case "clipboard_copy":
		s.handleClipboardCopy(c, data)
	case "board_get":
		s.handleBoardGet(c, data)
	case "webpane_open", "webpane_navigate", "webpane_back", "webpane_forward",
		"webpane_reload", "webpane_close", "webpane_input":
		s.handleWebPaneCommand(action, data)
	default:
		s.handleCommand(action, data)
	}
}
