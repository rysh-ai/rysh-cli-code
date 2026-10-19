// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
)

// E-86: `rysh meeting doctor` resolves the rysh-room keys from the session's
// secrets and config, and reports each as present or absent.
func TestMeetingDoctorReportsRoomHostKeys(t *testing.T) {
	h := newMeetingAgentsRig(t, nil) // secrets: OPENAI_API_KEY, RYSH_API_KEY, DEEPGRAM_API_KEY
	for _, k := range []string{meeting.LiveKitURLSecret, meeting.LiveKitKeySecret, meeting.LiveKitSecretSecret} {
		t.Setenv(k, "")
	}
	doctor := func() map[string]bool {
		c := meeting.Command{Action: "doctor"}
		c.Defaults()
		r := h.w.meeting.Do(c, h.w.meetingCredentials(c), nil)
		if r.State != "preflight" {
			t.Fatalf("%+v", r)
		}
		return r.Secrets
	}
	got := doctor()
	want := map[string]bool{
		meeting.RyshAPIKeySecret:    true,
		meeting.UpstreamAPIKeyName:  false,
		meeting.LiveKitURLSecret:    false,
		meeting.LiveKitKeySecret:    false,
		meeting.LiveKitSecretSecret: false,
		"DEEPGRAM_API_KEY":          true,
	}
	for n, v := range want {
		if present, ok := got[n]; !ok || present != v {
			t.Errorf("%s: got %v (reported %v), want %v", n, present, ok, v)
		}
	}

	h.w.cfg.Upstream.APIKey = "rysh_upstream"
	if got := doctor(); !got[meeting.UpstreamAPIKeyName] {
		t.Errorf("upstream.api_key set but reported absent: %v", got)
	}
}
