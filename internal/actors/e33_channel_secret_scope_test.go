// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E33 follow-up (founder live test 2026-10-28): after `rysh setup` gave the
// directory its own session (e33-live-4-<hex>), `##humanoid channel start
// assistant telegram` failed with "getMe failed: api error 404" while
// `rysh doctor` passed. setup, onboard and doctor keep a project's channel
// tokens in .rysh/secrets/default/ (config.LookupStoredSecretRef documents
// that as what a running daemon resolves), but the daemon looked only in its
// tab and workspace scopes — the workspace scope is the session name, so only
// a session literally named "default" found them. The unresolved
// ${TELEGRAM_BOT_TOKEN} was then sent to Telegram as the token itself.

const (
	e33Session  = "e33-live-4-abc123"
	e33TokenRef = "${E33_FAKE_BOT_TOKEN}"
	e33TokenVar = "E33_FAKE_BOT_TOKEN"
	e33FakeTok  = "e33-fake-bot-token"
)

// e33ProjectDir isolates cwd and $HOME and clears the reference from the
// environment, so only the files a test writes can resolve it.
func e33ProjectDir(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(e33TokenVar, "")
	_ = os.Unsetenv(e33TokenVar)
}

func e33WriteSecret(t *testing.T, scope, name, value string) {
	t.Helper()
	dir := filepath.Join(".rysh", "secrets", scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func e33Workspace() *WorkspaceActor {
	return &WorkspaceActor{sessionName: e33Session, secrets: newSecretStore(nil, nil)}
}

func TestE33NamedSessionResolvesProjectDefaultSecret(t *testing.T) {
	e33ProjectDir(t)
	e33WriteSecret(t, "default", e33TokenVar, e33FakeTok)

	if got := e33Workspace().namedExpandFunc("")(e33TokenRef); got != e33FakeTok {
		t.Fatalf("session %q left %s unresolved although .rysh/secrets/default holds it (the scope setup writes)", e33Session, e33TokenRef)
	}
}

func TestE33WorkspaceScopeStillBeatsProjectDefault(t *testing.T) {
	e33ProjectDir(t)
	e33WriteSecret(t, "default", e33TokenVar, "from-default")
	e33WriteSecret(t, e33Session, e33TokenVar, "from-workspace")

	if got := e33Workspace().namedExpandFunc("")(e33TokenRef); got != "from-workspace" {
		t.Fatal("the workspace scope's value did not win over the project default scope")
	}
	if chain := e33Workspace().secretScopeChain(""); len(chain) != 2 || chain[0] != e33Session || chain[1] != "default" {
		t.Fatalf("scope chain = %v, want [%s default]", chain, e33Session)
	}
	w := &WorkspaceActor{sessionName: "default"}
	if chain := w.secretScopeChain(""); len(chain) != 1 || chain[0] != "default" {
		t.Fatalf("a session named default: chain = %v, want [default] (no duplicate)", chain)
	}
}

// fakeTelegram answers getMe only for the fake token and 404s any other path,
// as the real Bot API does for a token it does not know. getUpdates waits
// briefly, then answers an empty batch: it must never wait on the client alone,
// or httptest.Server.Close blocks on a connection the stopped poller left open.
func fakeTelegram(t *testing.T) (url string, getMeCalls func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			mu.Lock()
			calls++
			mu.Unlock()
			if r.URL.Path != "/bot"+e33FakeTok+"/getMe" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"Fake","username":"FakeE33Bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

// e33AssistantTelegram writes an assistant profile the way setup does — the
// token as a ${NAME} reference — parses it through the workspace's expander,
// and returns the resolved telegram contact.
func e33AssistantTelegram(t *testing.T, apiBase string) msg.ChannelConfig {
	t.Helper()
	dir := filepath.Join(".rysh", "humanoids", "assistant")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: assistant\ncontacts:\n  telegram:\n    enabled: true\n    bot_token: " + e33TokenRef + "\n    api_base: " + apiBase + "\n---\nYou are the assistant.\n"
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := parseHumanoidFile(path, e33Workspace().namedExpandFunc(""))
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := def.Contacts["telegram"]
	if !ok {
		t.Fatal("profile has no telegram contact")
	}
	return cc
}

func e33Humanoid() (*HumanoidActor, *[]*msg.MsgHumanoidChannelStatus) {
	var got []*msg.MsgHumanoidChannelStatus
	h := &HumanoidActor{
		name:        "assistant",
		adapters:    map[string]channels.ChannelAdapter{},
		adapterCtxs: map[string]context.CancelFunc{},
	}
	h.channelStatusSink = func(s *msg.MsgHumanoidChannelStatus) { got = append(got, s) }
	return h, &got
}

func TestE33TelegramChannelStartsWithProjectSecretInNamedSession(t *testing.T) {
	e33ProjectDir(t)
	e33WriteSecret(t, "default", e33TokenVar, e33FakeTok)
	apiBase, getMeCalls := fakeTelegram(t)

	h, statuses := e33Humanoid()
	h.startChannel("telegram", e33AssistantTelegram(t, apiBase))
	defer h.stopChannel("telegram")

	if len(*statuses) != 1 || !(*statuses)[0].Connected {
		detail := ""
		if len(*statuses) > 0 {
			detail = (*statuses)[0].Error
		}
		t.Fatalf("telegram did not connect in session %q: %q", e33Session, detail)
	}
	if n := getMeCalls(); n != 1 {
		t.Fatalf("getMe called %d times, want 1", n)
	}
}

func TestE33ChannelStartRefusesUnresolvedCredentialRef(t *testing.T) {
	e33ProjectDir(t) // no secret anywhere
	apiBase, getMeCalls := fakeTelegram(t)

	h, statuses := e33Humanoid()
	h.startChannel("telegram", e33AssistantTelegram(t, apiBase))
	defer h.stopChannel("telegram")

	if _, running := h.adapters["telegram"]; running {
		t.Fatal("telegram started with an unresolved bot_token")
	}
	if n := getMeCalls(); n != 0 {
		t.Fatalf("getMe was sent %d time(s) with the unresolved reference as the token", n)
	}
	if len(*statuses) != 1 || (*statuses)[0].Connected {
		t.Fatalf("channel status = %+v, want exactly one not-connected report", *statuses)
	}
	errMsg := (*statuses)[0].Error
	for _, want := range []string{"bot_token", e33TokenRef, ".rysh/secrets/default/" + e33TokenVar} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("channel status error %q does not name %q", errMsg, want)
		}
	}
}
