// SPDX-License-Identifier: Apache-2.0

package channels

// security-lead M1 (2026-11-01) on the live Mattermost harness: the bot and
// echo tokens go to RYSH_LIVE_MATTERMOST_SERVER_URL, so that URL must be
// https, or plain http only to a loopback host (127.0.0.0/8, ::1,
// localhost). The live tests (build tag livechannels) stop with t.Fatal
// before any request when it is not; this file has no build tag, so the
// check itself runs in every `go test ./...`.

import (
	"strings"
	"testing"
)

// liveMattermostServerURLSafe refuses a server URL a token must not travel
// to: not https, plain http to a non-loopback host, user info, an unresolved
// ${…}, or empty. Errors never quote the value.
func liveMattermostServerURLSafe(raw string) error { return ValidateServerURL(raw) }

func TestLiveMattermostServerURLSafe(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://mattermost.example.com", true},
		{"https://mattermost.example.com:8443/", true},
		{"http://127.0.0.1:8065", true},
		{"http://[::1]:8065", true},
		{"http://localhost:8065", true},
		{"http://mattermost.example.com", false},
		{"http://127.0.0.1.nip.io:8065", false},
		{"http://10.0.0.5:8065", false},
		{"https://user:secret@mattermost.example.com", false},
		{"${MM_URL}", false},
		{"ftp://127.0.0.1/", false},
		{"", false},
	} {
		err := liveMattermostServerURLSafe(tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("%q: err = %v, want ok=%v", tc.url, err, tc.ok)
		}
		if err != nil && tc.url != "" && strings.Contains(err.Error(), tc.url) {
			t.Errorf("%q: error quotes the value: %v", tc.url, err)
		}
	}
}
