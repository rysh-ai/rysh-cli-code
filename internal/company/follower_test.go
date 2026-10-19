// SPDX-License-Identifier: Apache-2.0

package company

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestPollIntervalBacksOffOnceTheKeyIsRefused(t *testing.T) {
	if got := pollInterval(0, false); got != defaultPollEvery {
		t.Fatalf("default: %s", got)
	}
	if got := pollInterval(10*time.Second, false); got != 10*time.Second {
		t.Fatalf("configured: %s", got)
	}
	if got := pollInterval(10*time.Second, true); got != authFailedPollEvery {
		t.Fatalf("a refused key must poll slowly, got %s", got)
	}
}

func TestActivityRingIsNewestFirstAndCapped(t *testing.T) {
	var ring []msg.CompanyActivity
	for i := 0; i < 5; i++ {
		ring = pushActivity(ring, msg.CompanyActivity{Kind: "order", Reference: string(rune('A' + i))}, 3)
	}
	if len(ring) != 3 || ring[0].Reference != "E" || ring[2].Reference != "C" {
		t.Fatalf("ring: %+v", ring)
	}
	if ring[0].OccurredAt.IsZero() {
		t.Fatal("a hint without a time gets one")
	}
	merged := mergeActivity(ring, []msg.CompanyActivity{{Kind: "progress", Summary: "first"}, {Kind: "progress", Summary: "second"}})
	if len(merged) != 5 || merged[0].Reference != "E" || merged[3].Summary != "second" || merged[4].Summary != "first" {
		t.Fatalf("merge must put live hints first and replayed events newest-first after them: %+v", merged)
	}
}

func TestSubjectsAndProxyPath(t *testing.T) {
	if got := ActivitySubject("ws1", "c1"); got != "ws.ws1.ai-native-company.c1.activity" {
		t.Fatalf("activity subject: %s", got)
	}
	if got := ProgressSubject("ws1", "c1"); got != "ws.ws1.ai-native-company.c1.*.progress" {
		t.Fatalf("progress subject: %s", got)
	}
	if got := NATSWebSocketURL("https://letchat.ai/"); got != "wss://letchat.ai" {
		t.Fatalf("wss: %s", got)
	}
	if got := NATSWebSocketURL("http://localhost:8080"); got != "ws://localhost:8080" {
		t.Fatalf("ws: %s", got)
	}
	if got := ProxyPath("ws1"); got != "/workspaces/ws1/nats?connection_type=share" { // the key is a header now (F-86)
		t.Fatalf("proxy path: %s", got)
	}
}

func TestDescribeLink(t *testing.T) {
	cases := map[string]msg.CompanyLink{
		"key refused — reconnect from the dashboard": {AuthFailed: true},
		"disconnected: boom":                         {LastError: "boom"},
		"connecting…":                                {},
		"live":                                       {Connected: true, Live: true},
	}
	for want, link := range cases {
		if got := string(DescribeLink(link)); got != want {
			t.Errorf("DescribeLink(%+v) = %q, want %q", link, got, want)
		}
	}
	if got := string(DescribeLink(msg.CompanyLink{Connected: true})); got != "connected (polling every 1m0s)" {
		t.Fatalf("polling: %q", got)
	}
}
