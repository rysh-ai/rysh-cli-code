// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
)

// An external bus URL may carry credentials (nats://user:pass@host, or a token
// as the user, nats://token@host). The connect error is shown to the user and
// can reach logs, so it names the host and port but never the user info
// (security-lead note on the attach fix 6bb7ccc9; sprint 0.2.15 pull-forward).
// The values below are fixed test strings, not credentials.

const (
	redactTestUser   = "fixed-test-user-not-a-credential"
	redactTestSecret = "fixed-test-password-not-a-credential"
)

func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func assertNoUserInfo(t *testing.T, err error, port int) {
	t.Helper()
	if err == nil {
		t.Fatal("connect succeeded; want a refusal")
	}
	msg := err.Error()
	for _, leak := range []string{redactTestUser, redactTestSecret} {
		if strings.Contains(msg, leak) {
			t.Errorf("connect error carries the URL's user info: %q", msg)
		}
	}
	if !strings.Contains(msg, "127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("connect error %q no longer names the host and port", msg)
	}
}

func TestExternalConnectErrorHidesUserInfo(t *testing.T) {
	tokenHome(t)
	port := closedLoopbackPort(t)
	for _, userinfo := range []string{redactTestUser + ":" + redactTestSecret, redactTestSecret} {
		_, err := New(Config{Mode: "external", URL: "nats://" + userinfo + "@127.0.0.1:" + strconv.Itoa(port), SessionName: "s", Attach: true})
		assertNoUserInfo(t, err, port)
	}
}

func TestExternalMissingTokenErrorHidesUserInfo(t *testing.T) {
	tokenHome(t)
	owner, err := New(Config{Mode: "embedded", Port: 0, SessionName: "s", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	defer owner.Close()
	p, err := bustoken.Path(owner.ClientPort())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	port := owner.ClientPort()
	_, err = New(Config{Mode: "external", URL: "nats://" + redactTestUser + ":" + redactTestSecret + "@127.0.0.1:" + strconv.Itoa(port), SessionName: "s", Attach: true})
	assertNoUserInfo(t, err, port)
	if err != nil && !strings.Contains(err.Error(), "token") {
		t.Errorf("refusal %q no longer names the bus token", err)
	}
}

func TestShownBusURL(t *testing.T) {
	for raw, want := range map[string]string{
		"nats://127.0.0.1:4222":                          "nats://127.0.0.1:4222",
		"nats://u:p@127.0.0.1:4222":                      "nats://REDACTED@127.0.0.1:4222",
		"nats://tok@127.0.0.1:4222":                      "nats://REDACTED@127.0.0.1:4222",
		"tls://u:p@bus.example.test:4443/x":              "tls://REDACTED@bus.example.test:4443/x",
		"nats://u:p@127.0.0.1:4222,nats://v:q@127.0.0.1": "nats://REDACTED@127.0.0.1:4222,nats://REDACTED@127.0.0.1",
		"nats://u:p@127.0.0.1:4222,::bad":                "(unparseable bus URL)",
		"::not a url":                                    "(unparseable bus URL)",
	} {
		if got := shownBusURL(raw); got != want {
			t.Errorf("shownBusURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
