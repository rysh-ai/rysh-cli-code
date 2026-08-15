// SPDX-License-Identifier: Apache-2.0

package web

// Tests for the electronAPI-parity surface (web_electron_roadmap Phase 3):
// /api/env (W9), /api/workspaces (W8), /api/voice/* (W10) and the ws
// completion request/reply (W7) — including the access-token gate, which must
// cover /api/* exactly like every other route.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newParityTestServer builds a Server plus a gin router with only the parity
// API mounted (mirrors newControlTestServer).
func newParityTestServer(t *testing.T) (*Server, *gin.Engine) {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23232, "parity-test-session", pub, nc, pub.Codecs())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	s.registerParityAPI(r)
	return s, r
}

func getJSON(t *testing.T, r *gin.Engine, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body: %s)", path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: invalid JSON: %v", path, err)
	}
	return out
}

// TestEnvEndpointShape asserts /api/env carries the fields the renderer keys
// off: the is_web flag, platform, session name and the capability map (W9).
func TestEnvEndpointShape(t *testing.T) {
	s, r := newParityTestServer(t)
	s.SetWorkspaceInfo("/tmp/proj", "proj")

	env := getJSON(t, r, "/api/env")
	if env["is_web"] != true {
		t.Errorf("is_web = %v, want true", env["is_web"])
	}
	if env["platform"] != runtime.GOOS {
		t.Errorf("platform = %v, want %s", env["platform"], runtime.GOOS)
	}
	if env["session_name"] != "parity-test-session" {
		t.Errorf("session_name = %v", env["session_name"])
	}
	ws, _ := env["workspace"].(map[string]any)
	if ws == nil || ws["path"] != "/tmp/proj" || ws["name"] != "proj" {
		t.Errorf("workspace = %v", env["workspace"])
	}
	caps, _ := env["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatalf("capabilities missing: %v", env)
	}
	if caps["completion"] != true {
		t.Errorf("capabilities.completion = %v, want true", caps["completion"])
	}
	// No voice key configured ⇒ voice capability off, and the native-only
	// controls are always reported unavailable in web mode.
	if caps["voice"] != false || caps["restart_daemon"] != false || caps["native_open"] != false {
		t.Errorf("capabilities = %v", caps)
	}
	voice, _ := env["voice"].(map[string]any)
	if voice == nil || voice["enabled"] != false || voice["provider"] != "deepgram" || voice["hotkey"] != "ctrl+r" {
		t.Errorf("voice = %v", env["voice"])
	}
}

// TestWorkspacesEndpoint asserts /api/workspaces serves the current workspace
// and the injected recent list (W8).
func TestWorkspacesEndpoint(t *testing.T) {
	s, r := newParityTestServer(t)

	// Without any workspace info: current null, recent empty (not absent).
	out := getJSON(t, r, "/api/workspaces")
	if out["current"] != nil {
		t.Errorf("current = %v, want null", out["current"])
	}
	if rec, ok := out["recent"].([]any); !ok || len(rec) != 0 {
		t.Errorf("recent = %v, want []", out["recent"])
	}

	s.SetWorkspaceInfo("/home/u/proj", "") // name defaults to the base dir
	s.SetWorkspaceLister(func() []WorkspaceEntry {
		return []WorkspaceEntry{{Path: "/home/u/other", Name: "other", LastSession: "dev", LastOpened: "2026-07-25T00:00:00Z"}}
	})
	out = getJSON(t, r, "/api/workspaces")
	cur, _ := out["current"].(map[string]any)
	if cur == nil || cur["path"] != "/home/u/proj" || cur["name"] != "proj" {
		t.Errorf("current = %v", out["current"])
	}
	rec, _ := out["recent"].([]any)
	if len(rec) != 1 {
		t.Fatalf("recent = %v, want 1 entry", out["recent"])
	}
	first, _ := rec[0].(map[string]any)
	if first["path"] != "/home/u/other" || first["last_session"] != "dev" {
		t.Errorf("recent[0] = %v", first)
	}
}

// TestVoiceConfigEndpoint asserts /api/voice/config reports the configured
// provider/hotkey and NEVER leaks the API key (W10).
func TestVoiceConfigEndpoint(t *testing.T) {
	s, r := newParityTestServer(t)

	out := getJSON(t, r, "/api/voice/config")
	if out["enabled"] != false {
		t.Errorf("default enabled = %v, want false", out["enabled"])
	}

	const secret = "sk-super-secret-voice-key"
	s.SetVoice(VoiceSettings{Enabled: true, Provider: "whisper", APIKey: secret, Hotkey: "alt+v", Language: "en"})

	req := httptest.NewRequest(http.MethodGet, "/api/voice/config", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("/api/voice/config leaked the API key: %s", body)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(body), &cfg)
	if cfg["enabled"] != true || cfg["provider"] != "whisper" || cfg["hotkey"] != "alt+v" || cfg["language"] != "en" {
		t.Errorf("voice config = %v", cfg)
	}
}

// TestVoiceTranscribeEndpoint covers the transcription plumbing with an
// injected provider: body → temp file (extension from Content-Type) →
// transcript JSON; and the no-key error path (W10).
func TestVoiceTranscribeEndpoint(t *testing.T) {
	s, r := newParityTestServer(t)

	// No API key configured ⇒ visible error, not a silent no-op.
	req := httptest.NewRequest(http.MethodPost, "/api/voice/transcribe", strings.NewReader("audio-bytes"))
	req.Header.Set("Content-Type", "audio/webm")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if e, _ := out["error"].(string); !strings.Contains(e, "no API key") {
		t.Fatalf("expected no-API-key error, got %v", out)
	}

	// With a key + injected provider: audio bytes reach the provider via a
	// temp file whose extension reflects the uploaded MIME type.
	s.SetVoice(VoiceSettings{APIKey: "k", Provider: "deepgram"})
	var gotPath string
	var gotBytes []byte
	s.transcribeFn = func(_ context.Context, v VoiceSettings, audioPath string) (string, error) {
		gotPath = audioPath
		gotBytes, _ = os.ReadFile(audioPath)
		if v.APIKey != "k" {
			t.Errorf("transcribeFn got APIKey %q", v.APIKey)
		}
		return "hello world", nil
	}
	req = httptest.NewRequest(http.MethodPost, "/api/voice/transcribe", strings.NewReader("OPUSDATA"))
	req.Header.Set("Content-Type", "audio/webm;codecs=opus")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out = map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["transcript"] != "hello world" {
		t.Fatalf("transcript = %v (body %s)", out["transcript"], w.Body.String())
	}
	if !strings.HasSuffix(gotPath, ".webm") {
		t.Errorf("temp audio path = %q, want .webm suffix", gotPath)
	}
	if string(gotBytes) != "OPUSDATA" {
		t.Errorf("provider received %q", gotBytes)
	}
	if _, err := os.Stat(gotPath); !os.IsNotExist(err) {
		t.Errorf("temp audio file %s not cleaned up", gotPath)
	}

	// Empty body ⇒ visible error.
	req = httptest.NewRequest(http.MethodPost, "/api/voice/transcribe", strings.NewReader(""))
	req.Header.Set("Content-Type", "audio/webm")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out = map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if e, _ := out["error"].(string); !strings.Contains(e, "empty") {
		t.Errorf("expected empty-body error, got %v", out)
	}
}

func TestAudioExtForMime(t *testing.T) {
	cases := map[string]string{
		"audio/webm;codecs=opus": ".webm",
		"audio/ogg":              ".ogg",
		"audio/wav":              ".wav",
		"audio/mp4":              ".mp4",
		"audio/mpeg":             ".mp3",
		"":                       ".webm",
	}
	for mime, want := range cases {
		if got := audioExtForMime(mime); got != want {
			t.Errorf("audioExtForMime(%q) = %q, want %q", mime, got, want)
		}
	}
}

// TestParityAPILoginGate proves /api/* sits behind the same login middleware
// as the rest of the server: 401 without a login, 200 with one.
func TestParityAPILoginGate(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	creds, err := SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	token, err := signJWT(creds.SigningKey(), creds.Username, time.Now(), AccessTokenTTL)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	s := NewServer(port, "gate-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetCredentials(creds)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHTTP(t, base+"/health")
	client := &http.Client{Timeout: 3 * time.Second}

	for _, path := range []string{"/api/env", "/api/workspaces", "/api/voice/config"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a login = %d, want 401", path, resp.StatusCode)
		}

		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("GET %s with bearer: %v", path, err)
		}
		body := make([]byte, 1)
		_, _ = resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with a login = %d, want 200", path, resp.StatusCode)
		}
	}

	// POST transcribe is gated too.
	resp, err := client.Post(base+"/api/voice/transcribe", "audio/webm", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST transcribe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /api/voice/transcribe without a login = %d, want 401", resp.StatusCode)
	}
}

// dialWS connects a websocket client to a started Server, presenting a login
// JWT on the ?token= query when one is needed (the same path the browser uses,
// since a WebSocket handshake cannot carry an Authorization header).
func dialWS(t *testing.T, port int, token string) *websocket.Conn {
	t.Helper()
	url := fmt.Sprintf("ws://127.0.0.1:%d/ws?stream=0", port)
	if token != "" {
		url += "&token=" + token
	}
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", url, err, code)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readWSType reads frames until one of type want arrives (or the deadline
// hits, which fails the test).
func readWSType(t *testing.T, conn *websocket.Conn, want string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q frame: %v", want, err)
		}
		var envlp struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &envlp) != nil || envlp.Type != want {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(envlp.Data, &data); err != nil {
			t.Fatalf("decode %q data: %v", want, err)
		}
		return data
	}
}

// sendWSCommand sends a {type:"command"} frame the way the renderer does.
func sendWSCommand(t *testing.T, conn *websocket.Conn, action string, params map[string]any) {
	t.Helper()
	frame := map[string]any{
		"type": "command",
		"data": map[string]any{"action": action, "params": params},
	}
	if err := conn.WriteJSON(frame); err != nil {
		t.Fatalf("send %s: %v", action, err)
	}
}

// TestCompletionOverWS is the W7 round trip: a completion_get command over
// the live /ws protocol answers the requesting client — and only that client
// — with the matching candidates, directory entries marked.
func TestCompletionOverWS(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	// Control-mode posture (no login): this test is about the completion
	// protocol, and the gate has its own tests.
	s := NewServer(port, "ws-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	// A cwd with a known layout: completing "sub" must offer subdir/ + subfile.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	asker := dialWS(t, port, "")
	bystander := dialWS(t, port, "")

	sendWSCommand(t, asker, "completion_get", map[string]any{
		"request_id":     "req-1",
		"pane_id":        "pane-9",
		"token":          "sub",
		"line":           "cat sub",
		"cwd":            dir,
		"is_first_token": false,
	})

	data := readWSType(t, asker, "completion_result", 5*time.Second)
	if data["request_id"] != "req-1" || data["pane_id"] != "pane-9" {
		t.Fatalf("completion_result correlation = %v", data)
	}
	cands, _ := data["candidates"].([]any)
	got := map[string]bool{} // value → is_dir
	for _, c := range cands {
		m, _ := c.(map[string]any)
		got[m["value"].(string)], _ = m["is_dir"].(bool)
	}
	if !got["subdir"] {
		t.Errorf("candidates missing subdir (is_dir=true): %v", got)
	}
	if isDir, ok := got["subfile"]; !ok || isDir {
		t.Errorf("candidates missing subfile (is_dir=false): %v", got)
	}

	// Targeted reply: the bystander must NOT see another client's completion
	// (input lines are private).
	_ = bystander.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	for {
		_, raw, err := bystander.ReadMessage()
		if err != nil {
			break // timeout ⇒ nothing leaked
		}
		if strings.Contains(string(raw), "completion_result") {
			t.Fatalf("completion_result leaked to a non-requesting client: %s", raw)
		}
	}
}

// TestWebPaneUnavailableIsVisible (W12 fail-visible): with no workspace root
// (and thus no server-side browser), webpane_open must answer with an explicit
// webpane_error — never a silent no-op.
func TestWebPaneUnavailableIsVisible(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	s := NewServer(port, "wp-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	conn := dialWS(t, port, "")
	sendWSCommand(t, conn, "webpane_open", map[string]any{
		"pane_id": "pane-1", "url": "https://example.com", "profile": "default",
	})
	data := readWSType(t, conn, "webpane_error", 5*time.Second)
	if data["pane_id"] != "pane-1" {
		t.Errorf("webpane_error pane_id = %v", data["pane_id"])
	}
	if e, _ := data["error"].(string); !strings.Contains(e, "unavailable") {
		t.Errorf("webpane_error text = %q", e)
	}
}
