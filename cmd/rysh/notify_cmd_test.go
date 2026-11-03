// SPDX-License-Identifier: Apache-2.0

package main

// gap02 (G6): the daemon-side needs-you notifier's sinks, and `rysh notify test`.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/needsyou"
)

func TestNeedsYouSinksOffWithNoSink(t *testing.T) {
	sinks, err := buildNeedsYouSinks(config.Config{}, t.TempDir(), nil)
	if err != nil || len(sinks) != 0 {
		t.Fatalf("sinks=%v err=%v, want none", sinks, err)
	}
}

func TestNeedsYouSinksResolveWebhookRefWithoutPrintingIt(t *testing.T) {
	t.Setenv("GAP02_TEST_NOTIFY_URL", "http://127.0.0.1:1/topic-not-for-logs")
	cfg := config.Config{NotifyNeedsYou: config.NotifyNeedsYouConfig{Webhook: "${GAP02_TEST_NOTIFY_URL}", Command: []string{"x"}}}
	sinks, err := buildNeedsYouSinks(cfg, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sinks) != 2 || sinks[0].Name() != "webhook" || sinks[1].Name() != "command" {
		t.Fatalf("sinks = %v", sinks)
	}
}

func TestNeedsYouSinksMissingRefNamesOnlyTheName(t *testing.T) {
	cfg := config.Config{NotifyNeedsYou: config.NotifyNeedsYouConfig{Webhook: "https://example.invalid/${GAP02_TEST_UNSET_REF}"}}
	_, err := buildNeedsYouSinks(cfg, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "GAP02_TEST_UNSET_REF") {
		t.Fatalf("err = %v, want the missing name", err)
	}
	if strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("err carries the URL: %v", err)
	}
}

func TestNeedsYouSinksRejectNonHTTPWebhook(t *testing.T) {
	cfg := config.Config{NotifyNeedsYou: config.NotifyNeedsYouConfig{Webhook: "file:///etc/passwd"}}
	if _, err := buildNeedsYouSinks(cfg, t.TempDir(), nil); err == nil {
		t.Fatal("file: URL accepted")
	}
}

func TestNotifyTestSendsOneSyntheticAlert(t *testing.T) {
	got := make(chan map[string]any, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(b, &doc)
		got <- doc
	}))
	defer srv.Close()
	cfg := config.Config{SessionName: "work", NotifyNeedsYou: config.NotifyNeedsYouConfig{Webhook: srv.URL}}
	var out strings.Builder
	if err := runNotifyCmd(cfg, []string{"notify", "test"}, &out); err != nil {
		t.Fatalf("notify test: %v\n%s", err, out.String())
	}
	doc := <-got
	if doc["event"] != "needs_you" || doc["reason"] != "test" || doc["session"] != "work" {
		t.Fatalf("doc = %v", doc)
	}
	if !strings.Contains(out.String(), "webhook: sent") {
		t.Fatalf("output = %q", out.String())
	}
	if strings.Contains(out.String(), srv.URL) {
		t.Fatalf("output carries the URL: %q", out.String())
	}
}

func TestNotifyTestWithNoSinkFails(t *testing.T) {
	var out strings.Builder
	err := runNotifyCmd(config.Config{}, []string{"notify", "test"}, &out)
	if err == nil || !strings.Contains(err.Error(), "notify:") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotifyTestReportsAFailedSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := config.Config{NotifyNeedsYou: config.NotifyNeedsYouConfig{Webhook: srv.URL}}
	var out strings.Builder
	if err := runNotifyCmd(cfg, []string{"notify", "test"}, &out); err == nil {
		t.Fatalf("a 500 reported as sent: %s", out.String())
	}
}

// E33 slice E1 (design 086): the assistant sink is built when named, and
// `notify test` says what the humanoid answered.
type notifyFakeRequester struct{ reply interface{} }

func (f notifyFakeRequester) RequestFromSubject(string, interface{}, time.Duration) (interface{}, error) {
	return f.reply, nil
}

func TestNeedsYouSinksAssistantNeedsASession(t *testing.T) {
	cfg := config.Config{NotifyNeedsYou: config.NotifyNeedsYouConfig{Assistant: "assistant"}}
	if _, err := buildNeedsYouSinks(cfg, t.TempDir(), nil); err == nil {
		t.Fatal("assistant sink built with no session connection")
	}
	sinks, err := buildNeedsYouSinks(cfg, t.TempDir(), notifyFakeRequester{})
	if err != nil || len(sinks) != 1 || sinks[0].Name() != "assistant" {
		t.Fatalf("sinks=%v err=%v, want the assistant sink", sinks, err)
	}
}

func TestNotifyTestReportsTheAssistantAnswer(t *testing.T) {
	cfg := config.Config{SessionName: "work", NotifyNeedsYou: config.NotifyNeedsYouConfig{Assistant: "assistant"}}
	for _, c := range []struct {
		reply   *msg.MsgHumanoidNotifyOwnerReply
		wantErr bool
		want    string
	}{
		{reply: &msg.MsgHumanoidNotifyOwnerReply{Outcome: msg.NotifyOwnerSent}, want: "assistant: sent"},
		{reply: &msg.MsgHumanoidNotifyOwnerReply{Outcome: msg.NotifyOwnerNoChat, Error: "send the bot one direct message first"},
			wantErr: true, want: "assistant: FAILED"},
	} {
		prev := notifyAssistantRequester
		notifyAssistantRequester = func(config.Config) (needsyou.Requester, func(), error) {
			return notifyFakeRequester{reply: c.reply}, func() {}, nil
		}
		var out strings.Builder
		err := runNotifyCmd(cfg, []string{"notify", "test"}, &out)
		notifyAssistantRequester = prev
		if (err != nil) != c.wantErr || !strings.Contains(out.String(), c.want) {
			t.Fatalf("err=%v out=%q, want %q", err, out.String(), c.want)
		}
	}
}
