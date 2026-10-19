// SPDX-License-Identifier: Apache-2.0

package peer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPeerClientRefusesRedirectsAndUnsafeOrigins(t *testing.T) {
	for _, url := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/?key=secret", "https://example.com/path", "file:///tmp/peer"} {
		if ValidateURL(url) == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	for _, url := range []string{"https://rysh.ai", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if err := ValidateURL(url); err != nil {
			t.Fatal(err)
		}
	}
	secondCalled := false
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalled = true
		t.Error("redirect leaked a request")
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, second.URL, 302) }))
	defer first.Close()
	_, err := (Client{Connection: Connection{URL: first.URL}, Key: "rysh_secret"}).ResolveWorkspace(context.Background())
	if err == nil || !strings.Contains(err.Error(), "302") || secondCalled {
		t.Fatalf("redirect result: %v called=%t", err, secondCalled)
	}
}

func TestPeerConnectionPersistsSeparatelyAndDisplayIsInert(t *testing.T) {
	root := t.TempDir()
	a := Connection{Enabled: true, URL: "https://rysh.ai", KeySecret: "PEER_KEY", SessionID: "session-a"}
	if err := Save(root, "../same/name", a); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root, "../same/name")
	if err != nil || got != a {
		t.Fatalf("load: %+v %v", got, err)
	}
	b, err := Load(root, "other")
	if err != nil || b.Enabled {
		t.Fatalf("another session inherited peer config: %+v %v", b, err)
	}
	stat, err := os.Stat(connectionPath(root, "../same/name"))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", stat, err)
	}
	text := Display("hello\x1b]52;c;secret\x07\u009b2J\nworld")
	if strings.ContainsAny(text, "\x1b\x07\u009b") {
		t.Fatalf("terminal controls survived: %q", text)
	}
}

func TestPeerParserRejectsIgnoredFlags(t *testing.T) {
	for _, args := range [][]string{{"tail", "--limit", "0"}, {"tail", "--after", "-1"}, {"post", "--after", "1", "hello"}, {"connect", "http://example.com", "--key-secret", "KEY"}, {"reply", "bad", "hello"}, {"status", "--board", "team", "--board", "else"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	p, err := Parse([]string{"post", "--board", "team", "--", "literal --board elsewhere\n##cmd do-not-execute"})
	if err != nil || p.Text != "literal --board elsewhere\n##cmd do-not-execute" {
		t.Fatalf("text: %+v %v", p, err)
	}
}
