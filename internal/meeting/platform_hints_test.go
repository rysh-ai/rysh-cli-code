// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	zoomJoin     = "https://zoom.us/j/81234567890?pwd=x"
	teamsLiveURL = "https://teams.live.com/meet/9876543210987?p=XyZ123"
)

// Each platform names the host's own step while the bot waits (043 §"Zoom and
// Teams in panes").
func TestRecallWaitingNamesEachPlatformsStep(t *testing.T) {
	for _, tc := range []struct{ platform, code, want string }{
		{PlatformMeet, "in_waiting_room", "the host must admit it"},
		{PlatformZoom, "in_waiting_room", "Zoom waiting room"},
		{PlatformTeams, "in_waiting_room", "Teams lobby"},
		{PlatformTeams, "in_waiting_room", "(Unverified)"},
		{PlatformZoom, "in_call_not_recording", "allow its recording request"},
		{PlatformZoom, "recording_permission_allowed", "Allow recording"},
		{PlatformZoom, "recording_permission_denied", "denied"},
		{PlatformTeams, "recording_permission_denied", "denied"},
	} {
		if got := recallWaiting(tc.platform, tc.code); !strings.Contains(got, tc.want) {
			t.Errorf("%s/%s = %q, want it to contain %q", tc.platform, tc.code, got, tc.want)
		}
	}
	// Only Zoom asks the host for recording permission.
	for _, p := range []string{PlatformMeet, PlatformTeams} {
		if got := recallWaiting(p, "in_call_not_recording"); strings.Contains(got, "Zoom") {
			t.Errorf("%s: %q mentions Zoom", p, got)
		}
	}
	for _, code := range []string{"joining_call", "in_call_recording", "done"} {
		if got := recallWaiting(PlatformZoom, code); got != "" {
			t.Errorf("%s: %q, want no host action", code, got)
		}
	}
}

// End to end through the Recall status poll: a Teams bot in the lobby says
// "Teams lobby", a Zoom one "Zoom waiting room".
func TestJoiningNoticeNamesPlatformLobby(t *testing.T) {
	for url, want := range map[string]string{teamsJoin: "Teams lobby", teamsLiveURL: "Teams lobby", zoomJoin: "Zoom waiting room"} {
		s, _, _ := fixture(t, nil)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"status_changes":[{"code":"in_waiting_room"}]}`)
		}))
		s.recall = recallClient{provider.URL, "key", newHTTPClient()}
		s.mu.Lock()
		s.state, s.cfg.URL = "joining", url
		s.mu.Unlock()
		s.poll()
		r := s.status(false)
		provider.Close()
		if r.State != "joining" || !strings.Contains(r.Notice, want) {
			t.Errorf("%s: %+v", url, r)
		}
	}
}

func TestIsBotSpeakerAcrossPlatformLabels(t *testing.T) {
	for name, want := range map[string]bool{
		botName + " — say hey scribe":              true,
		botName + " — say hey scribe (Unverified)": true, // Teams, anonymous bot
		"  rysh ai — say hey planner":              true, // re-cased / padded
		"RYSH AI":                                  true,
		"Rysh":                                     false,
		"Halil Ağın":                               false,
		"":                                         false,
		"Unknown":                                  false,
	} {
		if got := isBotSpeaker(name); got != want {
			t.Errorf("isBotSpeaker(%q) = %v, want %v", name, got, want)
		}
	}
}

// The Teams "(Unverified)" bot name must not wake another agent bot.
func TestTeamsUnverifiedBotVoiceNeverWakes(t *testing.T) {
	s, _ := handsFree(t, func(context.Context, string) (string, error) { return "ok", nil }, "anyone")
	s.mu.Lock()
	s.cfg.URL = teamsJoin
	s.mu.Unlock()
	deliver(t, s, "self", said("hey rysh summarise", 9, botName+" — say hey planner (Unverified)", false, 1))
	if r := s.status(false); r.TurnID != "" || r.SegmentsHeard != 0 {
		t.Fatalf("a Teams bot voice was heard: %+v", r)
	}
}

// A Teams speaker without a caption identity is ignored under the default
// host allowlist, and the notice says why and how to fix it.
func TestIgnoredWakeNamesTeamsCaptionIdentity(t *testing.T) {
	for _, tc := range []struct {
		url, name string
		hint      bool
	}{
		{teamsJoin, "Unknown", true},
		{teamsLiveURL, "", true},
		{teamsJoin, "Visitor", false}, // named: the plain --allow notice is enough
		{zoomJoin, "Unknown", false},  // not a Teams caption issue
	} {
		s, _ := handsFree(t, func(context.Context, string) (string, error) { return "ok", nil }, "host")
		s.mu.Lock()
		s.cfg.URL = tc.url
		s.mu.Unlock()
		deliver(t, s, "w-"+tc.name, said("hey rysh summarise", 4, tc.name, false, 1))
		r := s.status(false)
		if r.TurnID != "" || r.IgnoredWakes != 1 || !strings.Contains(r.Notice, "not in --allow") {
			t.Fatalf("%s %q: %+v", tc.url, tc.name, r)
		}
		if got := strings.Contains(r.Notice, "identify me in meeting captions"); got != tc.hint {
			t.Errorf("%s %q: caption hint %v, want %v (%q)", tc.url, tc.name, got, tc.hint, r.Notice)
		}
	}
}

// A bot that Recall ends while joining (lobby timeout, Zoom refusal) says
// why, from Recall's sub_code; an odd sub_code is not echoed.
func TestEndedNoticeCarriesRecallSubCode(t *testing.T) {
	for body, want := range map[string]string{
		`{"status_changes":[{"code":"in_waiting_room"},{"code":"call_ended","sub_code":"timeout_exceeded_waiting_room"}]}`: "(Recall: timeout_exceeded_waiting_room)",
		`{"status_changes":[{"code":"fatal","sub_code":"zoom_join_failed"}]}`:                                              "(Recall: zoom_join_failed)",
		`{"status_changes":[{"code":"fatal","sub_code":"<script>"}]}`:                                                      "Meeting ended or bot was removed; leave",
		`{"status_changes":[{"code":"done","sub_code":null}]}`:                                                             "Meeting ended or bot was removed; leave",
	} {
		s, _, _ := fixture(t, nil)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		s.recall = recallClient{provider.URL, "key", newHTTPClient()}
		s.mu.Lock()
		s.state, s.cfg.URL = "joining", teamsJoin
		s.mu.Unlock()
		s.poll()
		r := s.status(false)
		provider.Close()
		if r.State != "ended" || !strings.Contains(r.Notice, want) {
			t.Errorf("%s: %+v", body, r)
		}
	}
}

// Once the host admits the bot, the lobby notice goes (Run T 2026-09-30: it
// stayed in status after admission).
func TestLobbyNoticeClearsOnAdmission(t *testing.T) {
	s, _, _ := fixture(t, nil)
	code := "in_waiting_room"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status_changes":[{"code":"`+code+`"}]}`)
	}))
	defer provider.Close()
	s.recall = recallClient{provider.URL, "key", newHTTPClient()}
	s.mu.Lock()
	s.state, s.cfg.URL = "joining", teamsJoin
	s.mu.Unlock()
	s.poll()
	if r := s.status(false); !strings.Contains(r.Notice, "Teams lobby") {
		t.Fatalf("waiting: %+v", r)
	}
	code = "in_call_recording"
	s.poll()
	if r := s.status(false); r.State != "listening" || r.Notice != "" {
		t.Fatalf("admitted: state %q notice %q", r.State, r.Notice)
	}
}
