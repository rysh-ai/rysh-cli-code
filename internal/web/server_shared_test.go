// SPDX-License-Identifier: Apache-2.0

package web

// The two-door rule: one web server, one session, two listeners with different
// postures. The desktop app's private loopback door stays open in control mode;
// the shared door always demands the login.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// startTwoDoorServer brings up a control-mode server (the desktop app's
// posture) with credentials stored, then shares it on a second port.
func startTwoDoorServer(t *testing.T) (private, shared int, creds *Credentials) {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	private, shared = freePort(t), freePort(t)
	var err error
	creds, err = SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer(private, "two-door", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true) // the app spawns its daemon this way
	s.SetCredentials(creds)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", private))

	if err := s.StartShared("127.0.0.1", shared); err != nil {
		t.Fatalf("StartShared: %v", err)
	}
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", shared))
	return private, shared, creds
}

func get(t *testing.T, url, bearer string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The whole point: same server, same session, opposite postures per door.
func TestSharedDoor_GatesWhilePrivateStaysOpen(t *testing.T) {
	private, shared, creds := startTwoDoorServer(t)

	if code := get(t, fmt.Sprintf("http://127.0.0.1:%d/api/env", private), ""); code != http.StatusOK {
		t.Errorf("private door with no credential = %d, want 200 (the desktop app presents none)", code)
	}
	if code := get(t, fmt.Sprintf("http://127.0.0.1:%d/api/env", shared), ""); code != http.StatusUnauthorized {
		t.Errorf("shared door with no credential = %d, want 401", code)
	}

	tok, err := signJWT(creds.SigningKey(), creds.Username, time.Now(), AccessTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	if code := get(t, fmt.Sprintf("http://127.0.0.1:%d/api/env", shared), tok); code != http.StatusOK {
		t.Errorf("shared door with a login = %d, want 200", code)
	}
}

// The login page must load unauthenticated on the shared door, or nobody could
// ever sign in; /health stays public on both.
func TestSharedDoor_ServesLoginPageAndHealth(t *testing.T) {
	private, shared, _ := startTwoDoorServer(t)
	for _, port := range []int{private, shared} {
		if code := get(t, fmt.Sprintf("http://127.0.0.1:%d/", port), ""); code != http.StatusOK {
			t.Errorf("GET / on :%d = %d, want 200", port, code)
		}
		if code := get(t, fmt.Sprintf("http://127.0.0.1:%d/health", port), ""); code != http.StatusOK {
			t.Errorf("GET /health on :%d = %d, want 200", port, code)
		}
	}
}

// /api/auth/status answers for the door it was asked on, so the app is never
// shown a password form for a connection that needs none.
func TestSharedDoor_AuthStatusIsPerDoor(t *testing.T) {
	private, shared, _ := startTwoDoorServer(t)
	body := func(url string) string {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		return string(buf[:n])
	}
	if got := body(fmt.Sprintf("http://127.0.0.1:%d/api/auth/status", private)); !contains(got, `"login_required":false`) {
		t.Errorf("private door status = %s, want login_required:false", got)
	}
	if got := body(fmt.Sprintf("http://127.0.0.1:%d/api/auth/status", shared)); !contains(got, `"login_required":true`) {
		t.Errorf("shared door status = %s, want login_required:true", got)
	}
}

// Closing the shared door must leave the server, and everyone on the private
// door, untouched.
func TestSharedDoor_StopSharedLeavesServerRunning(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	private, shared := freePort(t), freePort(t)
	creds, err := SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(private, "two-door", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	s.SetCredentials(creds)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", private))

	if err := s.StartShared("127.0.0.1", shared); err != nil {
		t.Fatalf("StartShared: %v", err)
	}
	if h, p := s.SharedAddr(); h != "127.0.0.1" || p != shared {
		t.Fatalf("SharedAddr = %s:%d, want 127.0.0.1:%d", h, p, shared)
	}
	if err := s.StopShared(); err != nil {
		t.Fatalf("StopShared: %v", err)
	}
	if _, p := s.SharedAddr(); p != 0 {
		t.Errorf("SharedAddr after close = %d, want 0", p)
	}
	if !s.IsRunning() {
		t.Error("closing the shared door must not stop the server")
	}
	if code := get(t, fmt.Sprintf("http://127.0.0.1:%d/health", private), ""); code != http.StatusOK {
		t.Errorf("private door after closing the shared one = %d, want 200", code)
	}
}

// A shared address is never opened without a login to check against.
func TestSharedDoor_RefusedWithoutCredentials(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	private, shared := freePort(t), freePort(t)

	s := NewServer(private, "two-door", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", private))

	if err := s.StartShared("127.0.0.1", shared); err == nil {
		t.Fatal("StartShared with no credentials must fail — it would serve the session to anyone")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// F-9: a second daemon rotating the credentials file must not lock this one out.
//
// rysh state is project-local — there is no ~/.rysh — so EVERY daemon rooted at
// a workspace shares one web-auth.json. The server used to take a snapshot of it
// at start and never look again, so when another daemon ran `##rysh web auth`
// (which mints a fresh HS256 key by design, to log old browsers out) this
// server kept verifying against the key nobody was minting with any more.
//
// Measured in the wild on 2026-08-07: 127 WebSocket dials over ngrok and 180
// local, all 401, zero connections, until `##rysh web stop --shared` + `start`
// — which is exactly what proves the listener only read the file at start.
//
// The fix is not "stop rotating": a second daemon is entitled to set the
// workspace's login. It is that a running server must follow the file.
func TestSharedDoorFollowsARotatedCredentialsFile(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	private, shared := freePort(t), freePort(t)

	// One workspace rysh dir, shared by every daemon rooted here.
	ryshDir := t.TempDir()
	first, err := SaveCredentials(ryshDir, "halil", "s3cret")
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer(private, "rotated", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	s.TrackCredentialsFile(ryshDir)
	s.SetCredentials(first)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", private))
	if err := s.StartShared("127.0.0.1", shared); err != nil {
		t.Fatalf("StartShared: %v", err)
	}
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", shared))

	api := fmt.Sprintf("http://127.0.0.1:%d/api/env", shared)
	tokFirst, err := signJWT(first.SigningKey(), first.Username, time.Now(), AccessTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	if code := get(t, api, tokFirst); code != http.StatusOK {
		t.Fatalf("before rotation, a token from the stored login = %d, want 200", code)
	}

	// ANOTHER DAEMON sets the workspace's login. Same file, different process —
	// this one is never told.
	second, err := SaveCredentials(ryshDir, "halil", "rotated-by-the-other-daemon")
	if err != nil {
		t.Fatal(err)
	}
	if second.Secret == first.Secret {
		t.Fatal("rotation did not mint a new signing key — the premise of this test is gone")
	}

	tokSecond, err := signJWT(second.SigningKey(), second.Username, time.Now(), AccessTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	if code := get(t, api, tokSecond); code != http.StatusOK {
		t.Errorf("after another daemon rotated the key, a token minted from the CURRENT "+
			"credentials file = %d, want 200 — this is F-9: every client is locked out until "+
			"the listener restarts", code)
	}
	// The other half of the same behaviour, and the reason rotation exists: the
	// OLD key must stop working, or changing the password would mean nothing.
	if code := get(t, api, tokFirst); code != http.StatusUnauthorized {
		t.Errorf("after rotation, a token signed with the SUPERSEDED key = %d, want 401 — "+
			"rotation must still log every old browser out", code)
	}
}
