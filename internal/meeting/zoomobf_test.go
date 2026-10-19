// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

const zoomJoinURL = "https://zoom.us/j/81234567890?pwd=x"

func TestZoomOBFValidate(t *testing.T) {
	c := Command{URL: zoomJoinURL, PublicURL: "https://meeting.example", Consent: true, ZoomOBF: true}
	c.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://meet.google.com/abc-defg-hij", "https://acme.zoom.us/my/jane.doe", "https://teams.live.com/meet/9876543210987?p=XyZ123"} {
		c.URL = u
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--zoom-obf") {
			t.Errorf("%s: %v", u, err)
		}
	}
	for u, want := range map[string]string{zoomJoinURL: "81234567890", "https://app.zoom.us/wc/join/81234567890": "81234567890", "https://zoom.us/wc/81234567890/join": "81234567890"} {
		if got, ok := zoomMeetingNumber(u); !ok || got != want {
			t.Errorf("%s: %q %v", u, got, ok)
		}
	}
}

// Payload carries zoom.obf_token_url only with --zoom-obf, and a join with
// incomplete Zoom secrets fails before any provider call.
func TestZoomOBFJoinPayloadAndMissingSecrets(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	var bodies []map[string]any
	var calls atomic.Int32
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		bodies = append(bodies, p)
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})
	keys := Credentials{Recall: "key", Verification: "whsec_" + base64.StdEncoding.EncodeToString(testKey), Speech: "speech"}
	exec := func(context.Context, string) (string, error) { return "", nil }
	join := Command{Action: "join", URL: zoomJoinURL, PublicURL: "https://meeting.example/p", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true, ZoomOBF: true}
	m := NewManager()
	t.Cleanup(m.Close)
	if r := m.Do(join, keys, exec); !strings.Contains(r.Error, ZoomRefreshTokenSecret) || calls.Load() != 0 {
		t.Fatalf("missing Zoom secrets: %+v, calls %d", r, calls.Load())
	}
	keys.Zoom = ZoomCredentials{ClientID: "id", ClientSecret: "sec", RefreshToken: "r1"}
	if r := m.Do(join, keys, exec); r.State != "create_failed" {
		t.Fatal(r)
	}
	join.ZoomOBF = false
	if r := m.Do(join, keys, exec); r.State != "create_failed" {
		t.Fatal(r)
	}
	if len(bodies) != 2 {
		t.Fatal(len(bodies))
	}
	zoom, _ := bodies[0]["zoom"].(map[string]any)
	cb, _ := zoom["obf_token_url"].(string)
	if !regexp.MustCompile(`^https://meeting\.example/p/zoom/obf\?auth_token=[0-9a-f]{64}$`).MatchString(cb) {
		t.Fatalf("obf_token_url %q", cb)
	}
	if strings.Contains(cb, "sec") || strings.Contains(cb, "r1") {
		t.Fatal("Zoom secret in callback URL")
	}
	if _, ok := bodies[1]["zoom"]; ok {
		t.Fatal("zoom block sent without --zoom-obf")
	}
}

type fakeZoom struct {
	refreshes, mints atomic.Int32
	refresh          string
	apiStatus        int
}

func (f *fakeZoom) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			id, sec, ok := r.BasicAuth()
			_ = r.ParseForm()
			if !ok || id != "id" || sec != "sec" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != f.refresh {
				w.WriteHeader(401)
				return
			}
			n := f.refreshes.Add(1)
			f.refresh = "r" + string(rune('1'+n))
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc", "refresh_token": f.refresh, "expires_in": 3599})
		case "/v2/users/me/token":
			q := r.URL.Query()
			if r.Header.Get("Authorization") != "Bearer acc" || q.Get("type") != "onbehalf" || q.Get("meeting_id") != "81234567890" {
				w.WriteHeader(400)
				return
			}
			if f.apiStatus != 0 {
				w.WriteHeader(f.apiStatus)
				_, _ = io.WriteString(w, `{"message":"token acc leaked"}`)
				return
			}
			f.mints.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "obf-token"})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func obfService(t *testing.T, f *fakeZoom, saved *[]string) (*Service, *zoomOBF) {
	s, _, _ := fixture(t, nil)
	srv := f.server(t)
	z, err := newZoomOBF(zoomJoinURL, ZoomCredentials{ClientID: "id", ClientSecret: "sec", RefreshToken: f.refresh, SaveRefresh: func(v string) error { *saved = append(*saved, v); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	z.oauthURL, z.apiURL = srv.URL+"/oauth/token", srv.URL+"/v2"
	s.mu.Lock()
	s.obf = z
	s.mu.Unlock()
	return s, z
}

func obfGet(s *Service, method, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(method, "https://meeting.example/zoom/obf?auth_token="+token, nil))
	return w
}

func TestZoomOBFCallbackMintsRotatesAndCaches(t *testing.T) {
	f := &fakeZoom{refresh: "r1"}
	var saved []string
	s, z := obfService(t, f, &saved)
	if w := obfGet(s, "GET", strings.Repeat("0", 64)); w.Code != 401 {
		t.Fatal("wrong capability:", w.Code)
	}
	if w := obfGet(s, "GET", ""); w.Code != 401 {
		t.Fatal("missing capability:", w.Code)
	}
	if w := obfGet(s, "POST", z.capability); w.Code != 405 {
		t.Fatal("POST:", w.Code)
	}
	for i := 0; i < 2; i++ {
		w := obfGet(s, "GET", z.capability)
		if w.Code != 200 || w.Body.String() != "obf-token" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("mint %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	if f.refreshes.Load() != 1 || f.mints.Load() != 2 {
		t.Fatalf("refreshes %d mints %d: access token must be cached", f.refreshes.Load(), f.mints.Load())
	}
	if len(saved) != 1 || saved[0] != "r2" || z.creds.RefreshToken != "r2" {
		t.Fatalf("rotated refresh token not saved: %v", saved)
	}
	if n := s.status(false).Notice; strings.Contains(n, "OBF") {
		t.Fatal("unexpected notice:", n)
	}
}

func TestZoomOBFFailuresNeverLeakAndAreBounded(t *testing.T) {
	f := &fakeZoom{refresh: "r1", apiStatus: 403}
	var saved []string
	s, z := obfService(t, f, &saved)
	w := obfGet(s, "GET", z.capability)
	if w.Code != 502 || w.Body.Len() != 0 {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	n := s.status(false).Notice
	if !strings.Contains(n, "HTTP 403") || strings.Contains(n, "acc") || strings.Contains(n, "r2") {
		t.Fatal("notice:", n)
	}
	for i := 1; i < zoomOBFMaxMints; i++ {
		obfGet(s, "GET", z.capability)
	}
	f.apiStatus = 0
	if w := obfGet(s, "GET", z.capability); w.Code != 502 || f.mints.Load() != 0 {
		t.Fatal("mint limit not enforced")
	}
	s.mu.Lock()
	s.terminal = true
	s.mu.Unlock()
	if w := obfGet(s, "GET", z.capability); w.Code != 410 {
		t.Fatal("ended meeting still mints:", w.Code)
	}
}

func TestZoomOBFRefreshRejectedAndUnsavedRotation(t *testing.T) {
	f := &fakeZoom{refresh: "other"}
	var saved []string
	s, z := obfService(t, f, &saved)
	z.creds.RefreshToken = "stale"
	if w := obfGet(s, "GET", z.capability); w.Code != 502 || !strings.Contains(s.status(false).Notice, "re-authorise") {
		t.Fatal(w.Code, s.status(false).Notice)
	}
	f2 := &fakeZoom{refresh: "r1"}
	s2, z2 := obfService(t, f2, &saved)
	z2.creds.SaveRefresh = func(string) error { return io.ErrClosedPipe }
	if w := obfGet(s2, "GET", z2.capability); w.Code != 200 || !strings.Contains(s2.status(false).Notice, "could not be saved") {
		t.Fatal(w.Code, s2.status(false).Notice)
	}
}

func TestZoomOBFEndpointAbsentByDefault(t *testing.T) {
	s, _, _ := fixture(t, nil)
	if w := obfGet(s, "GET", "x"); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
