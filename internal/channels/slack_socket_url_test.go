// SPDX-License-Identifier: Apache-2.0

package channels

// Socket Mode connects to the URL apps.connections.open returns. Real Slack
// returns wss://; a plain ws:// URL is accepted only for a loopback host (the
// local fake platform). Anything else is refused before slack-go dials it, and
// the error names the scheme and host only: the URL's query carries the
// connection ticket. No test here dials: the guard wraps a canned client.

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

type cannedDoer struct{ body string }

func (c cannedDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

func guardDo(t *testing.T, path, body string) (string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://slack.com/api/"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := slackSocketURLGuard{next: cannedDoer{body}}.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestSlackSocketURLGuard(t *testing.T) {
	const ticket = "ticket=synthetic-ticket-x"
	for _, tc := range []struct {
		name, url string
		ok        bool
	}{
		{"wss to slack", "wss://wss-primary.slack.com/link/?" + ticket, true},
		{"ws to 127.0.0.1", "ws://127.0.0.1:39001/link/?" + ticket, true},
		{"ws to localhost", "ws://localhost:39001/link/?" + ticket, true},
		{"ws to ::1", "ws://[::1]:39001/link/?" + ticket, true},
		{"ws to a remote host", "ws://wss-primary.slack.com/link/?" + ticket, false},
		{"ws to 0.0.0.0", "ws://0.0.0.0:39001/link/?" + ticket, false},
		{"another scheme", "http://wss-primary.slack.com/link/?" + ticket, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"ok":true,"url":"` + tc.url + `"}`
			got, err := guardDo(t, "apps.connections.open", body)
			if tc.ok {
				if err != nil || got != body {
					t.Fatalf("allowed URL: err %v, body %q; want the response unchanged", err, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("a %s Socket Mode URL was let through", tc.name)
			}
			if strings.Contains(err.Error(), "synthetic-ticket") {
				t.Errorf("the error carries the URL's ticket: %v", err)
			}
			if !strings.Contains(err.Error(), "wss://") {
				t.Errorf("the error should say Socket Mode needs wss://: %v", err)
			}
		})
	}
}

// Other API methods, and an error answer without a URL, pass through unchanged.
func TestSlackSocketURLGuardLeavesOtherCallsAlone(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"auth.test", `{"ok":true,"user_id":"U1","url":"http://example.slack.com/"}`},
		{"apps.connections.open", `{"ok":false,"error":"invalid_auth"}`},
	} {
		got, err := guardDo(t, tc.path, tc.body)
		if err != nil || got != tc.body {
			t.Errorf("%s: err %v, body %q; want it unchanged", tc.path, err, got)
		}
	}
}

// Start's API client (shared by the reconnect loop) carries the guard. A
// behavioural test would need the unfixed code to dial a non-loopback host, so
// the wiring line is pinned in the source instead (as for D-55 B).
func TestSlackStartWiresTheSocketURLGuard(t *testing.T) {
	src, err := os.ReadFile("slack.go")
	if err != nil {
		t.Fatal(err)
	}
	start := regexp.MustCompile(`(?s)func \(s \*SlackAdapter\) Start\(.*?\n}\n`).Find(src)
	if start == nil {
		t.Fatal("SlackAdapter.Start not found in slack.go — if it moved, move this test with it")
	}
	if !strings.Contains(string(start), "slack.OptionHTTPClient(slackSocketURLGuard{") {
		t.Error("Start does not give its Slack API client the slackSocketURLGuard HTTP client")
	}
}
