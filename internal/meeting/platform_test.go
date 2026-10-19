// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const teamsJoin = "https://teams.microsoft.com/l/meetup-join/19%3ameeting_NjQ1MjBkYzItYjU0Yi00ZDIxLWE4ZGEtOTc0YzQ5ZjQ2ZmZh%40thread.v2/0?context=%7b%22Tid%22%3a%2272f988bf-86f1-41af-91ab-2d7cd011db47%22%2c%22Oid%22%3a%22a1b2c3d4-0000-4000-8000-000000000000%22%7d"

type urlVectors struct {
	Accepted []struct{ URL, Platform, Why string } `json:"accepted"`
	Refused  []struct{ URL, Why string }           `json:"refused"`
}

// testdata/meeting_urls.json is shared with rysh-server so both sides keep
// the same allowlist; change the file, not a copy.
func loadURLVectors(t *testing.T) urlVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/meeting_urls.json")
	if err != nil {
		t.Fatal(err)
	}
	var v urlVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Accepted) == 0 || len(v.Refused) == 0 {
		t.Fatal("empty URL vectors")
	}
	return v
}

func TestMeetingPlatformAcceptsEachPlatform(t *testing.T) {
	for _, c := range loadURLVectors(t).Accepted {
		got, err := MeetingPlatform(c.URL)
		if err != nil || got != c.Platform {
			t.Errorf("%s (%s): got %q, %v; want %q", c.URL, c.Why, got, err, c.Platform)
		}
	}
}

func TestMeetingPlatformRejectsArbitraryAndCredentialLinks(t *testing.T) {
	for _, c := range loadURLVectors(t).Refused {
		if p, err := MeetingPlatform(c.URL); err == nil {
			t.Errorf("accepted %q (%s) as %s", c.URL, c.Why, p)
		}
	}
}

func TestValidateUsesPlatformAllowlist(t *testing.T) {
	c := Command{URL: "https://zoom.us/j/81234567890?pwd=x", PublicURL: "https://meeting.example", Consent: true}
	c.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.URL = "https://zoom.us/s/81234567890"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "Zoom") {
		t.Fatal("start link accepted or unhelpful error:", err)
	}
}

// Recall infers the platform from meeting_url; the create payload is the
// same for every platform and must carry the URL verbatim, with no
// platform-specific credential blocks (zak/OBF) we do not implement.
func TestCreatePayloadPerPlatform(t *testing.T) {
	for _, raw := range []string{"https://meet.google.com/abc-defg-hij", "https://us06web.zoom.us/j/81234567890?pwd=AbC.1", teamsJoin, "https://teams.live.com/meet/9876543210987?p=XyZ123"} {
		c := Command{URL: raw, PublicURL: "https://meeting.example", Consent: true}
		c.Defaults()
		if err := c.Validate(); err != nil {
			t.Fatal(raw, err)
		}
		b, _ := json.Marshal(createPayload(c, "capability"))
		var p map[string]any
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if p["meeting_url"] != raw {
			t.Errorf("meeting_url altered: %v", p["meeting_url"])
		}
		for _, k := range []string{"zoom", "microsoft_teams", "google_meet"} {
			if _, ok := p[k]; ok {
				t.Errorf("%s: unexpected platform block %q", raw, k)
			}
		}
		om := p["output_media"].(map[string]any)["camera"].(map[string]any)
		if om["kind"] != "webpage" || !strings.HasSuffix(om["config"].(map[string]any)["url"].(string), "/media#capability") {
			t.Errorf("%s: output media page missing: %v", raw, om)
		}
		leave := p["automatic_leave"].(map[string]any)
		// Recall caps the Teams lobby wait at 30 minutes regardless of config.
		if w := leave["waiting_room_timeout"].(float64); w < 60 || w > 1800 {
			t.Errorf("waiting_room_timeout %v outside 60–1800", w)
		}
	}
}

// While joining, the status names what the bot waits for: Zoom's recording
// request and the Teams/Zoom lobby are steps the host must act on.
func TestJoiningNoticeNamesHostAction(t *testing.T) {
	for code, want := range map[string]string{
		"in_waiting_room":             "admit",
		"in_call_not_recording":       "allow its recording request",
		"recording_permission_denied": "denied",
	} {
		s, _, _ := fixture(t, nil)
		body := `{"status_changes":[{"code":"` + code + `"}]}`
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		s.recall = recallClient{provider.URL, "key", newHTTPClient()}
		s.mu.Lock()
		s.state, s.cfg.URL = "joining", "https://zoom.us/j/81234567890?pwd=x"
		s.mu.Unlock()
		s.poll()
		r := s.status(false)
		provider.Close()
		if r.State != "joining" || !strings.Contains(r.Notice, want) || r.Platform != PlatformZoom {
			t.Errorf("%s: %+v", code, r)
		}
	}
}
