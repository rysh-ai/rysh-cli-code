// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// D-10 / E31-R piece 6 (lead ruling on msg-0106): when rysh-server refuses
// the channel-credentials fetch with 403 (an observer's key), the humanoid
// channel does NOT start; the refusal is reported (ERROR log + channel
// status) with the server's message and the fix. Any other failure (5xx,
// timeout) keeps today's behaviour: WARN, start without the token.

const credsRefusal = "channel credentials need the member role or higher; this key belongs to an observer"

func humanoidAgainst(t *testing.T, status int, body string) (*HumanoidActor, *[]*msg.MsgHumanoidChannelStatus, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	var cfg config.Config
	cfg.Upstream.Enabled = true
	cfg.Upstream.URL = srv.URL
	cfg.Upstream.APIKey = "rysh_test_observer_key"
	cfg.Upstream.Workspace = "ws-1"
	var got []*msg.MsgHumanoidChannelStatus
	h := &HumanoidActor{name: "test-humanoid", cfg: cfg}
	h.channelStatusSink = func(s *msg.MsgHumanoidChannelStatus) { got = append(got, s) }
	return h, &got, &calls
}

func TestHumanoidChannelRefusesToStartOnCredentials403(t *testing.T) {
	h, statuses, calls := humanoidAgainst(t, http.StatusForbidden, `{"error":"`+credsRefusal+`"}`)
	// A slack channel declared without tokens: the server holds them.
	h.startChannel("slack", msg.ChannelConfig{Enabled: true})

	if *calls != 1 {
		t.Fatalf("credentials fetched %d times, want 1", *calls)
	}
	if _, running := h.adapters["slack"]; running {
		t.Fatal("the slack channel started after a 403 — it must not start without its credentials")
	}
	if len(*statuses) != 1 || (*statuses)[0].Connected {
		t.Fatalf("channel status = %+v, want exactly one not-connected report", *statuses)
	}
	errMsg := (*statuses)[0].Error
	for _, want := range []string{credsRefusal, "ask a workspace admin to change your role, or use a member key"} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("channel status error %q does not contain %q", errMsg, want)
		}
	}
}

func TestHumanoidCredentials500KeepsTodaysBehaviour(t *testing.T) {
	h, _, calls := humanoidAgainst(t, http.StatusInternalServerError, `{"error":"boom"}`)
	cc, refused := h.resolveChannelCredentialsFromUpstream("slack", msg.ChannelConfig{Enabled: true})
	if *calls != 1 {
		t.Fatalf("credentials fetched %d times, want 1", *calls)
	}
	if refused != nil {
		t.Fatalf("500 refused the channel (%v); it must WARN and continue without the token", refused)
	}
	if cc.BotToken != "" || cc.AppToken != "" {
		t.Errorf("500 produced tokens: %+v", cc)
	}
}
