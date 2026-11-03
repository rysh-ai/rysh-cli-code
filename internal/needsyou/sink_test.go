// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var testAlert = Alert{
	Session:  "work",
	Host:     "devbox",
	PaneID:   "0123456789abcdef",
	PaneName: "builder",
	Reason:   "approval",
	Since:    1700000000000,
}

func TestAlertTextHasNoScreenAndNamesThePane(t *testing.T) {
	got := testAlert.Text()
	want := "rysh: builder needs you (approval) — session work on devbox"
	if got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
}

func TestWebhookPostsTheFixedDocument(t *testing.T) {
	type req struct {
		method, ctype string
		body          []byte
	}
	got := make(chan req, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- req{r.Method, r.Header.Get("Content-Type"), b}
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL)
	if err := s.Send(context.Background(), testAlert); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := <-got
	if r.method != http.MethodPost || r.ctype != "application/json" {
		t.Fatalf("method %s, content-type %q", r.method, r.ctype)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	var keys []string
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "event,host,pane,pane_name,reason,session,since,text" {
		t.Fatalf("keys = %v (the document is a fixed set; add, never leak)", keys)
	}
	if doc["event"] != "needs_you" || doc["pane"] != "0123456789abcdef" || doc["reason"] != "approval" || doc["since"] != float64(1700000000000) {
		t.Fatalf("doc = %v", doc)
	}
}

func TestWebhookNon2xxIsAnErrorWithoutTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	u := srv.URL + "/secret-topic-123"
	err := NewWebhookSink(u).Send(context.Background(), testAlert)
	if err == nil {
		t.Fatal("403 accepted")
	}
	if strings.Contains(err.Error(), "secret-topic-123") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error carries the URL: %v", err)
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	hit := make(chan bool, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if err := NewWebhookSink(srv.URL).Send(context.Background(), testAlert); err == nil {
		t.Fatal("a redirect was treated as delivered")
	}
	select {
	case <-hit:
		t.Fatal("redirect followed")
	default:
	}
}

// The command sink runs this test binary itself as the "program" (no stub on
// PATH, nothing named like a real tool).
func TestMain(m *testing.M) {
	if out := os.Getenv("NEEDSYOU_TEST_HELPER_OUT"); out != "" {
		var b strings.Builder
		for _, k := range []string{"RYSH_NOTIFY_EVENT", "RYSH_NOTIFY_SESSION", "RYSH_NOTIFY_HOST", "RYSH_NOTIFY_PANE",
			"RYSH_NOTIFY_PANE_NAME", "RYSH_NOTIFY_REASON", "RYSH_NOTIFY_SINCE", "RYSH_NOTIFY_TEXT"} {
			b.WriteString(k + "=" + os.Getenv(k) + "\n")
		}
		b.WriteString("ARGS=" + strings.Join(os.Args[1:], "|") + "\n")
		_ = os.WriteFile(out, []byte(b.String()), 0o600)
		if os.Getenv("NEEDSYOU_TEST_HELPER_FAIL") != "" {
			os.Exit(3)
		}
		if os.Getenv("NEEDSYOU_TEST_HELPER_HANG") != "" {
			time.Sleep(time.Minute)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCommandSinkRunsArgvWithNotifyEnvAndNoShell(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.txt")
	t.Setenv("NEEDSYOU_TEST_HELPER_OUT", out)
	s := NewCommandSink([]string{os.Args[0], "a b", "$(touch pwned)"})
	if err := s.Send(context.Background(), testAlert); err != nil {
		t.Fatalf("send: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"RYSH_NOTIFY_EVENT=needs_you\n", "RYSH_NOTIFY_SESSION=work\n", "RYSH_NOTIFY_HOST=devbox\n",
		"RYSH_NOTIFY_PANE=0123456789abcdef\n", "RYSH_NOTIFY_PANE_NAME=builder\n", "RYSH_NOTIFY_REASON=approval\n",
		"RYSH_NOTIFY_SINCE=1700000000000\n", "RYSH_NOTIFY_TEXT=" + testAlert.Text() + "\n",
		"ARGS=a b|$(touch pwned)\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}

func TestCommandSinkNonZeroExitIsAnError(t *testing.T) {
	t.Setenv("NEEDSYOU_TEST_HELPER_OUT", filepath.Join(t.TempDir(), "o"))
	t.Setenv("NEEDSYOU_TEST_HELPER_FAIL", "1")
	if err := NewCommandSink([]string{os.Args[0]}).Send(context.Background(), testAlert); err == nil {
		t.Fatal("exit 3 accepted")
	}
}

func TestCommandSinkIsBoundedByTheContext(t *testing.T) {
	t.Setenv("NEEDSYOU_TEST_HELPER_OUT", filepath.Join(t.TempDir(), "o"))
	t.Setenv("NEEDSYOU_TEST_HELPER_HANG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := NewCommandSink([]string{os.Args[0]}).Send(ctx, testAlert); err == nil {
		t.Fatal("hung command reported delivered")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("context did not bound the command")
	}
}
