// SPDX-License-Identifier: Apache-2.0

package channels

// gap04 (root-order-074, G16): "@example_bot on a GitHub issue or PR → the agent in
// your rysh session does the work → it replies on the thread". Everything here
// runs against a local fake GitHub (httptest) and the synthetic fixtures in
// testdata/github — no real App, no real token, no network.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeGitHub is a recording stand-in for the GitHub REST API.
type fakeGitHub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []fakeGitHubReq
	// login is the account GET /user names (the token's owner); userStatus,
	// when set, is the status GET /user answers with instead (a bad token).
	login      string
	userStatus int
}

type fakeGitHubReq struct {
	Method string
	Path   string
	Auth   bool // an Authorization: Bearer header was present (never its value)
	Body   map[string]any
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{login: "example_bot"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeGitHubReq{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && len(r.Header.Get("Authorization")) > len("Bearer "),
			Body:   body,
		})
		login, userStatus := f.login, f.userStatus
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user" && userStatus != 0:
			w.WriteHeader(userStatus)
			_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"login":"`+login+`","id":77,"type":"User"}`)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":1}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) requests() []fakeGitHubReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeGitHubReq(nil), f.reqs...)
}

// waitFor polls until pred holds over the recorded requests, or fails.
func (f *fakeGitHub) waitFor(t *testing.T, what string, pred func([]fakeGitHubReq) bool) []fakeGitHubReq {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.requests(); pred(r) {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake GitHub never saw %s; got %+v", what, f.requests())
	return nil
}

// testSecret is generated per test run, so no fixed secret-shaped literal
// lives in the source.
func testSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func githubSign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// fixture loads testdata/github/<name>.json and applies edits (dotted paths).
func fixture(t *testing.T, name string, edits map[string]any) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/github/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for path, v := range edits {
		parts := strings.Split(path, ".")
		node := doc
		for _, p := range parts[:len(parts)-1] {
			node = node[p].(map[string]any)
		}
		node[parts[len(parts)-1]] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type ghHarness struct {
	a      *GitHubAdapter
	fake   *fakeGitHub
	secret string
	n      int
}

// freeGitHubPort returns a loopback port nothing is listening on, so a test
// that calls Start never binds the default webhook port.
func freeGitHubPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func newGitHubHarness(t *testing.T, mutate func(*msg.ChannelConfig)) *ghHarness {
	t.Helper()
	fake := newFakeGitHub(t)
	secret := testSecret(t)
	cfg := msg.ChannelConfig{
		Enabled:   true,
		BotToken:  "test-token-not-real",
		AppSecret: secret,
		BotLogin:  "example_bot",
		Channels:  []string{"acme/widgets"},
		APIBase:   fake.srv.URL,
		// A pairing code must never be needed on a public thread, so the
		// channel only starts with pairing_policy drop or an allowlist.
		PairingPolicy: "drop",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &ghHarness{a: NewGitHubAdapter(cfg), fake: fake, secret: secret}
}

// deliver POSTs a signed webhook to the adapter's handler.
func (h *ghHarness) deliver(t *testing.T, event string, body []byte) int {
	t.Helper()
	h.n++
	return h.deliverWith(t, event, body, githubSign(body, h.secret), "delivery-"+strconv.Itoa(h.n))
}

func (h *ghHarness) deliverWith(t *testing.T, event string, body []byte, sig, delivery string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	h.a.handleWebhook(rec, req)
	return rec.Code
}

func (h *ghHarness) inbound(t *testing.T) (InboundMessage, bool) {
	t.Helper()
	select {
	case m := <-h.a.InboundCh():
		return m, true
	case <-time.After(150 * time.Millisecond):
		return InboundMessage{}, false
	}
}

func (h *ghHarness) mustInbound(t *testing.T) InboundMessage {
	t.Helper()
	m, ok := h.inbound(t)
	if !ok {
		t.Fatal("expected an inbound message, got none")
	}
	return m
}

func (h *ghHarness) mustNoInbound(t *testing.T, why string) {
	t.Helper()
	if m, ok := h.inbound(t); ok {
		t.Fatalf("%s: expected no inbound, got %+v", why, m)
	}
}

func TestGitHubIsABuiltInChannel(t *testing.T) {
	if !IsValidChannelType("github") {
		t.Fatal(`"github" must be a valid built-in channel type`)
	}
	a, err := NewAdapter("github", msg.ChannelConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewAdapter(github): %v", err)
	}
	if _, ok := a.(*GitHubAdapter); !ok {
		t.Fatalf("NewAdapter(github) = %T, want *GitHubAdapter", a)
	}
	if a.Type() != "github" {
		t.Fatalf("Type() = %q", a.Type())
	}
	var _ IdentityValidator = (*GitHubAdapter)(nil)
}

func TestGitHubStartFailsClosed(t *testing.T) {
	cases := map[string]func(*msg.ChannelConfig){
		"no webhook secret":   func(c *msg.ChannelConfig) { c.AppSecret = "" },
		"unresolved secret":   func(c *msg.ChannelConfig) { c.AppSecret = "${GITHUB_WEBHOOK_SECRET}" },
		"no token":            func(c *msg.ChannelConfig) { c.BotToken = "" },
		"unresolved token":    func(c *msg.ChannelConfig) { c.BotToken = "${GITHUB_BOT_TOKEN}" },
		"no repos":            func(c *msg.ChannelConfig) { c.Channels = nil },
		"repo not owner/name": func(c *msg.ChannelConfig) { c.Channels = []string{"widgets"} },
		// R7: a pairing request would ask a stranger for a code on a public
		// thread, so only drop, or an allowlist, may start.
		"pairing request, no allowlist":    func(c *msg.ChannelConfig) { c.PairingPolicy = "request" },
		"pairing open, no allowlist":       func(c *msg.ChannelConfig) { c.PairingPolicy = "open" },
		"pairing unset, no allowlist":      func(c *msg.ChannelConfig) { c.PairingPolicy = "" },
		"pairing request, blank allowlist": func(c *msg.ChannelConfig) { c.PairingPolicy = "request"; c.Allowlist = []string{" ", ""} },
		// G-4: no default trigger handle. "rysh" is someone else's GitHub
		// account and "ryshagent" is unowned, so the handle must be set.
		"no bot_login":         func(c *msg.ChannelConfig) { c.BotLogin = "" },
		"blank bot_login":      func(c *msg.ChannelConfig) { c.BotLogin = "  " },
		"bot_login only @":     func(c *msg.ChannelConfig) { c.BotLogin = "@" },
		"unresolved bot_login": func(c *msg.ChannelConfig) { c.BotLogin = "${GITHUB_BOT_LOGIN}" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newGitHubHarness(t, func(c *msg.ChannelConfig) {
				c.WebhookPort = freeGitHubPort(t)
				mutate(c)
			})
			if err := h.a.Start(context.Background()); err == nil {
				_ = h.a.Stop()
				t.Fatal("Start must refuse")
			}
		})
	}
	// The token's account must be known before any delivery is read (it is
	// "self"), so a token GitHub rejects stops Start too.
	t.Run("token rejected by GitHub", func(t *testing.T) {
		h := newGitHubHarness(t, func(c *msg.ChannelConfig) { c.WebhookPort = freeGitHubPort(t) })
		h.fake.mu.Lock()
		h.fake.userStatus = http.StatusUnauthorized
		h.fake.mu.Unlock()
		if err := h.a.Start(context.Background()); err == nil {
			_ = h.a.Stop()
			t.Fatal("Start must refuse when GET /user fails")
		}
	})
}

func TestGitHubStartAcceptsAllowlistWithPairingRequest(t *testing.T) {
	h := newGitHubHarness(t, func(c *msg.ChannelConfig) {
		c.WebhookPort = freeGitHubPort(t)
		c.PairingPolicy = "request"
		c.Allowlist = []string{"alice"}
	})
	if err := h.a.Start(context.Background()); err != nil {
		t.Fatalf("Start with an allowlist: %v", err)
	}
	_ = h.a.Stop()
}

// TestGitHubTokenAccountIsSelf: the docs let the reply token belong to an
// ordinary account whose login differs from bot_login. Its approval footer
// says "@example_bot yes", so if that account is an OWNER/MEMBER/COLLABORATOR its own
// comment would trigger the bot. Start resolves the token's login and treats
// it as self.
func TestGitHubTokenAccountIsSelf(t *testing.T) {
	h := newGitHubHarness(t, func(c *msg.ChannelConfig) { c.WebhookPort = freeGitHubPort(t) })
	h.fake.mu.Lock()
	h.fake.login = "acme-ops"
	h.fake.mu.Unlock()
	if err := h.a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.a.Stop()
	reqs := h.fake.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/user" || !reqs[0].Auth {
		t.Fatalf("Start must resolve the token's account with one GET /user; got %+v", reqs)
	}

	// The reply account posts the approval notice; GitHub delivers it back.
	notice := "[approval required] bash: go test ./...\n\nOn GitHub, answer with a mention: `@example_bot yes` or `@example_bot no`."
	for _, login := range []string{"acme-ops", "ACME-OPS"} {
		code := h.deliver(t, "issue_comment", fixture(t, "issue_comment_created", map[string]any{
			"comment.body":               notice,
			"comment.author_association": "OWNER",
			"comment.user":               map[string]any{"login": login, "type": "User"},
			"sender":                     map[string]any{"login": login, "type": "User"},
		}))
		if code != http.StatusOK {
			t.Fatalf("webhook = %d", code)
		}
		h.mustNoInbound(t, "the reply account's own comment ("+login+")")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(h.fake.requests()); n != 1 {
		t.Fatalf("the bot reacted to its own comment (%d requests after GET /user)", n-1)
	}
	// Anyone else still triggers it.
	h.deliver(t, "issue_comment", fixture(t, "issue_comment_created", nil))
	h.mustInbound(t)
}

func TestGitHubStartBindsLoopbackOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	h := newGitHubHarness(t, func(c *msg.ChannelConfig) { c.WebhookPort = strconv.Itoa(port) })
	if err := h.a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.a.Stop()
	if got := h.a.listenAddr(); got != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("listener on %q, want loopback 127.0.0.1:%d", got, port)
	}
	if !h.a.Status().Connected {
		t.Fatal("Status().Connected = false after Start")
	}
	// The listener serves the webhook: an unsigned POST is refused, not 404.
	resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(port)+"/", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned POST over the listener = %d, want 401", resp.StatusCode)
	}
}

func TestGitHubWebhookRejectsUnsignedAndBadSignature(t *testing.T) {
	h := newGitHubHarness(t, nil)
	body := fixture(t, "issue_comment_created", nil)
	if code := h.deliverWith(t, "issue_comment", body, "", "d-1"); code != http.StatusUnauthorized {
		t.Fatalf("unsigned = %d, want 401", code)
	}
	if code := h.deliverWith(t, "issue_comment", body, githubSign(body, "wrong-secret"), "d-2"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", code)
	}
	if code := h.deliverWith(t, "issue_comment", body, "sha1=abcdef", "d-3"); code != http.StatusUnauthorized {
		t.Fatalf("sha1 signature = %d, want 401", code)
	}
	h.mustNoInbound(t, "unsigned/bad signature")
	if n := len(h.fake.requests()); n != 0 {
		t.Fatalf("a rejected webhook reached GitHub %d times", n)
	}
}

func TestGitHubWebhookRejectsGET(t *testing.T) {
	h := newGitHubHarness(t, nil)
	rec := httptest.NewRecorder()
	h.a.handleWebhook(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}

func TestGitHubPingIsAcknowledged(t *testing.T) {
	h := newGitHubHarness(t, nil)
	if code := h.deliver(t, "ping", fixture(t, "ping", nil)); code != http.StatusOK {
		t.Fatalf("ping = %d, want 200", code)
	}
	h.mustNoInbound(t, "ping")
}

func TestGitHubIssueCommentMentionBecomesInbound(t *testing.T) {
	h := newGitHubHarness(t, nil)
	if code := h.deliver(t, "issue_comment", fixture(t, "issue_comment_created", nil)); code != http.StatusOK {
		t.Fatalf("webhook = %d, want 200", code)
	}
	m := h.mustInbound(t)
	if m.SenderID != "alice" || m.SenderName != "alice" {
		t.Errorf("sender = %q/%q, want alice", m.SenderID, m.SenderName)
	}
	if m.ThreadID != "acme/widgets#42" {
		t.Errorf("ThreadID = %q, want acme/widgets#42", m.ThreadID)
	}
	// Content is the comment minus the mention and nothing else: a reply of
	// "@example_bot yes" must arrive as exactly "yes", or an approval held on this
	// thread could never be released from GitHub. Context rides in Metadata.
	if m.Content != "please find why the parser drops the last token" {
		t.Errorf("Content = %q, want the comment without the mention", m.Content)
	}
	md := m.Metadata
	if md["repo"] != "acme/widgets" || md["number"] != "42" || md["kind"] != "issue" || md["comment_id"] != "9001" || md["delivery"] == "" {
		t.Errorf("metadata = %+v", md)
	}
	if md["url"] != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("context metadata = %+v", md)
	}
	// R7: the title can be written by anyone who opened the issue, so it is
	// not carried toward the prompt.
	if _, ok := md["title"]; ok {
		t.Errorf("metadata carries the issue title: %+v", md)
	}
	if _, ok := md["channel"]; ok {
		t.Errorf("metadata must not carry a 'channel' key (the humanoid would misroute the reply)")
	}
	// The commenter sees it was picked up: one 👀 reaction on their comment.
	reqs := h.fake.waitFor(t, "an eyes reaction", func(r []fakeGitHubReq) bool { return len(r) >= 1 })
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/repos/acme/widgets/issues/comments/9001/reactions" || r.Body["content"] != "eyes" || !r.Auth {
		t.Fatalf("reaction request = %+v", r)
	}
}

func TestGitHubCommentOnPullRequestIsAPullRequestThread(t *testing.T) {
	h := newGitHubHarness(t, nil)
	h.deliver(t, "issue_comment", fixture(t, "issue_comment_on_pr_created", nil))
	m := h.mustInbound(t)
	if m.ThreadID != "acme/widgets#7" || m.Metadata["kind"] != "pull_request" {
		t.Fatalf("thread/kind = %q/%q", m.ThreadID, m.Metadata["kind"])
	}
	if m.Content != "review this PR" {
		t.Errorf("Content = %q", m.Content)
	}
}

func TestGitHubReviewCommentMentionBecomesInbound(t *testing.T) {
	h := newGitHubHarness(t, nil)
	h.deliver(t, "pull_request_review_comment", fixture(t, "pull_request_review_comment_created", nil))
	m := h.mustInbound(t)
	if m.ThreadID != "acme/widgets#7" || m.Metadata["kind"] != "pull_request" {
		t.Fatalf("thread/kind = %q/%q", m.ThreadID, m.Metadata["kind"])
	}
	if m.Content != "is this off by one?" {
		t.Errorf("Content = %q", m.Content)
	}
	if md := m.Metadata; md["comment_id"] != "9101" || md["url"] == "" {
		t.Errorf("review metadata = %+v", md)
	}
	// R7: the PR title, its branch name and the file path are the PR author's
	// text, and the PR author may be a stranger; none is carried.
	for _, k := range []string{"title", "path", "line", "head_ref"} {
		if _, ok := m.Metadata[k]; ok {
			t.Errorf("review metadata carries %q: %+v", k, m.Metadata)
		}
	}
	reqs := h.fake.waitFor(t, "an eyes reaction", func(r []fakeGitHubReq) bool { return len(r) >= 1 })
	if reqs[0].Path != "/repos/acme/widgets/pulls/comments/9101/reactions" {
		t.Fatalf("review-comment reaction path = %q", reqs[0].Path)
	}
}

func TestGitHubIssueOpenedWithMentionBecomesInbound(t *testing.T) {
	h := newGitHubHarness(t, nil)
	h.deliver(t, "issues", fixture(t, "issues_opened", nil))
	m := h.mustInbound(t)
	if m.ThreadID != "acme/widgets#43" || !strings.Contains(m.Content, "can you reproduce and propose a fix?") || !strings.Contains(m.Content, "Steps: run with no args.") {
		t.Fatalf("inbound = %+v", m)
	}
	if _, ok := m.Metadata["comment_id"]; ok {
		t.Errorf("an opened issue has no comment id: %+v", m.Metadata)
	}
	reqs := h.fake.waitFor(t, "an eyes reaction", func(r []fakeGitHubReq) bool { return len(r) >= 1 })
	if reqs[0].Path != "/repos/acme/widgets/issues/43/reactions" {
		t.Fatalf("issue reaction path = %q", reqs[0].Path)
	}
}

func TestGitHubIgnoresWhatItMustIgnore(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		fixture string
		edits   map[string]any
	}{
		{"no mention", "issue_comment", "issue_comment_created", map[string]any{"comment.body": "looks fine to me"}},
		{"longer handle", "issue_comment", "issue_comment_created", map[string]any{"comment.body": "@example_bot-bot fix it"}},
		{"email address", "issue_comment", "issue_comment_created", map[string]any{"comment.body": "mail ops@example_bot.ai about it"}},
		{"package name", "issue_comment", "issue_comment_created", map[string]any{"comment.body": "try @example_bot/code-reviewer"}},
		{"edited comment", "issue_comment", "issue_comment_created", map[string]any{"action": "edited"}},
		{"deleted comment", "issue_comment", "issue_comment_created", map[string]any{"action": "deleted"}},
		{"unlisted repo", "issue_comment", "issue_comment_created", map[string]any{"repository.full_name": "evil/fork"}},
		{"stranger (NONE)", "issue_comment", "issue_comment_created", map[string]any{"comment.author_association": "NONE"}},
		{"contributor", "issue_comment", "issue_comment_created", map[string]any{"comment.author_association": "CONTRIBUTOR"}},
		{"first-timer", "issue_comment", "issue_comment_created", map[string]any{"comment.author_association": "FIRST_TIME_CONTRIBUTOR"}},
		{"missing association", "issue_comment", "issue_comment_created", map[string]any{"comment.author_association": ""}},
		{"own comment (loop)", "issue_comment", "issue_comment_created", map[string]any{"comment.user": map[string]any{"login": "example_bot", "type": "User"}, "sender": map[string]any{"login": "example_bot", "type": "User"}}},
		{"own app bot (loop)", "issue_comment", "issue_comment_created", map[string]any{"comment.user": map[string]any{"login": "example_bot[bot]", "type": "Bot"}, "sender": map[string]any{"login": "example_bot[bot]", "type": "Bot"}}},
		{"other bot", "issue_comment", "issue_comment_created", map[string]any{"sender": map[string]any{"login": "dependabot[bot]", "type": "Bot"}}},
		{"issue edited", "issues", "issues_opened", map[string]any{"action": "edited"}},
		{"issue by stranger", "issues", "issues_opened", map[string]any{"issue.author_association": "NONE"}},
		{"review comment edited", "pull_request_review_comment", "pull_request_review_comment_created", map[string]any{"action": "edited"}},
		{"unhandled event", "push", "issue_comment_created", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGitHubHarness(t, nil)
			code := h.deliver(t, tc.event, fixture(t, tc.fixture, tc.edits))
			if code < 200 || code > 299 {
				t.Fatalf("signed but ignored webhook = %d, want 2xx (GitHub would retry)", code)
			}
			h.mustNoInbound(t, tc.name)
			time.Sleep(50 * time.Millisecond)
			if n := len(h.fake.requests()); n != 0 {
				t.Fatalf("an ignored webhook still called GitHub %d times", n)
			}
		})
	}
}

func TestGitHubMentionMatchesCaseInsensitiveAndPunctuated(t *testing.T) {
	for _, body := range []string{"@EXAMPLE_BOT fix it", "hey @example_bot, fix it", "(@example_bot) fix it", "fix it @example_bot."} {
		h := newGitHubHarness(t, nil)
		h.deliver(t, "issue_comment", fixture(t, "issue_comment_created", map[string]any{"comment.body": body}))
		if _, ok := h.inbound(t); !ok {
			t.Errorf("body %q did not trigger", body)
		}
	}
}

func TestGitHubRepoMatchIsCaseInsensitive(t *testing.T) {
	h := newGitHubHarness(t, func(c *msg.ChannelConfig) { c.Channels = []string{"Acme/Widgets"} })
	h.deliver(t, "issue_comment", fixture(t, "issue_comment_created", nil))
	h.mustInbound(t)
}

func TestGitHubDuplicateDeliveryHandledOnce(t *testing.T) {
	h := newGitHubHarness(t, nil)
	body := fixture(t, "issue_comment_created", nil)
	sig := githubSign(body, h.secret)
	h.deliverWith(t, "issue_comment", body, sig, "same-delivery")
	h.mustInbound(t)
	if code := h.deliverWith(t, "issue_comment", body, sig, "same-delivery"); code != http.StatusOK {
		t.Fatalf("redelivery = %d, want 200", code)
	}
	h.mustNoInbound(t, "redelivery of the same X-GitHub-Delivery")
	// A redelivery with a new delivery id but the same comment is the same
	// request too: GitHub's "Redeliver" button issues a new id.
	h.deliverWith(t, "issue_comment", body, sig, "another-delivery")
	h.mustNoInbound(t, "same comment id under a new delivery id")
}

func TestGitHubSendPostsIssueComment(t *testing.T) {
	h := newGitHubHarness(t, nil)
	err := h.a.Send(context.Background(), OutboundMessage{RecipientID: "alice", ThreadID: "acme/widgets#42", Content: "Found it: the loop stops one short."})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := h.fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %+v", reqs)
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/repos/acme/widgets/issues/42/comments" || !r.Auth {
		t.Fatalf("request = %+v", r)
	}
	if r.Body["body"] != "Found it: the loop stops one short." {
		t.Fatalf("body = %+v", r.Body)
	}
}

func TestGitHubSendTellsHowToAnswerAnApproval(t *testing.T) {
	// The humanoid's approval notice says 'reply "yes"'; on GitHub only a
	// mention reaches the session, so the posted notice must say so.
	h := newGitHubHarness(t, nil)
	notice := "[approval required] bash: go test ./...\nReply \"yes\" to approve, \"no\" to reject."
	if err := h.a.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: notice}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := h.fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 comment, got %d", len(reqs))
	}
	body, _ := reqs[0].Body["body"].(string)
	if !strings.HasPrefix(body, notice) || !strings.Contains(body, "`@example_bot yes`") || !strings.Contains(body, "`@example_bot no`") {
		t.Fatalf("approval notice on GitHub = %q", body)
	}
	// An ordinary reply is posted verbatim.
	_ = h.a.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: "done"})
	if b, _ := h.fake.requests()[1].Body["body"].(string); b != "done" {
		t.Fatalf("ordinary reply altered: %q", b)
	}
}

func TestGitHubSendSkipsProgressSteps(t *testing.T) {
	h := newGitHubHarness(t, nil)
	if err := h.a.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: "Reading parser.go", Kind: OutboundKindStep}); err != nil {
		t.Fatalf("Send(step): %v", err)
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Fatalf("a progress step was posted as %d comment(s)", n)
	}
}

func TestGitHubSendSplitsLongReplies(t *testing.T) {
	h := newGitHubHarness(t, nil)
	long := strings.Repeat("x", githubMaxBodyLen+10)
	if err := h.a.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: long}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := h.fake.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 comments, got %d", len(reqs))
	}
	total := 0
	for _, r := range reqs {
		s, _ := r.Body["body"].(string)
		if len(s) > githubMaxBodyLen {
			t.Fatalf("chunk of %d chars exceeds the limit", len(s))
		}
		total += len(s)
	}
	if total != len(long) {
		t.Fatalf("split lost content: %d of %d", total, len(long))
	}
}

func TestGitHubSendRefusesBadThreadAndUnlistedRepo(t *testing.T) {
	h := newGitHubHarness(t, nil)
	for _, thread := range []string{"", "acme/widgets", "acme/widgets#", "acme/widgets#x", "widgets#4", "evil/fork#1"} {
		if err := h.a.Send(context.Background(), OutboundMessage{ThreadID: thread, Content: "hi"}); err == nil {
			t.Errorf("Send to thread %q must fail", thread)
		}
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Fatalf("refused sends still reached GitHub %d times", n)
	}
}

func TestGitHubSendSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	}))
	defer srv.Close()
	h := newGitHubHarness(t, func(c *msg.ChannelConfig) { c.APIBase = srv.URL })
	err := h.a.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("err = %v, want the 403 and GitHub's message", err)
	}
}

func TestGitHubValidateIdentity(t *testing.T) {
	h := newGitHubHarness(t, nil)
	id, err := h.a.ValidateIdentity(context.Background())
	if err != nil {
		t.Fatalf("ValidateIdentity: %v", err)
	}
	if id.Username != "example_bot" || id.ID != "77" {
		t.Fatalf("identity = %+v", id)
	}
	reqs := h.fake.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/user" || !reqs[0].Auth {
		t.Fatalf("validate requests = %+v", reqs)
	}

	unresolved := newGitHubHarness(t, func(c *msg.ChannelConfig) { c.BotToken = "${GITHUB_BOT_TOKEN}" })
	if err := unresolved.a.Validate(context.Background()); err == nil {
		t.Fatal("Validate must refuse an unresolved ${…} token")
	}
	if n := len(unresolved.fake.requests()); n != 0 {
		t.Fatalf("an unresolved token was sent to GitHub (%d requests)", n)
	}
}

// countingTransport stands in for the network: it records that a request was
// attempted (and whether it carried the token) and answers with an error, so
// a refused api_base can be shown to send nothing anywhere.
type countingTransport struct {
	mu    sync.Mutex
	n     int
	authd int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if r.Header.Get("Authorization") != "" {
		c.authd++
	}
	return nil, io.ErrUnexpectedEOF
}

// Round 2 item (1), R2: the token rides in every request, so api_base is
// checked with channels.ValidateAPIBase before Start, Validate or Send sends
// anything — https, or http on a loopback host only; no user info; no
// look-alike loopback names.
func TestGitHubRefusesBadAPIBaseBeforeAnyRequest(t *testing.T) {
	bad := map[string]string{
		"http on a non-loopback host": "http://ghe.example.com/api/v3",
		"localhost look-alike":        "http://localhost.evil.com",
		"127.0.0.1 look-alike":        "http://127.0.0.1.nip.io",
		"127.0.0.1 as user info":      "http://127.0.0.1@evil.com",
		"user info on https":          "https://x-token@ghe.example.com/api/v3",
		"user info on loopback":       "http://user:pw@127.0.0.1:9",
		"not an absolute URL":         "ghe.example.com/api/v3",
		"other scheme":                "ftp://ghe.example.com",
		"unresolved reference":        "${GITHUB_API_BASE}",
	}
	for name, base := range bad {
		t.Run(name, func(t *testing.T) {
			h := newGitHubHarness(t, func(c *msg.ChannelConfig) {
				c.WebhookPort = freeGitHubPort(t)
				c.APIBase = base
			})
			rt := &countingTransport{}
			h.a.client.Transport = rt

			if err := h.a.Start(context.Background()); err == nil {
				_ = h.a.Stop()
				t.Error("Start must refuse")
			} else if strings.Contains(err.Error(), base) {
				t.Error("the Start error quotes the api_base value")
			}
			if err := h.a.Validate(context.Background()); err == nil {
				t.Error("Validate must refuse")
			}
			if err := h.a.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#7", Content: "hi"}); err == nil {
				t.Error("Send must refuse")
			}
			rt.mu.Lock()
			n, authd := rt.n, rt.authd
			rt.mu.Unlock()
			if n != 0 || authd != 0 {
				t.Errorf("requests attempted = %d (%d with the token), want 0", n, authd)
			}
			if got := len(h.fake.requests()); got != 0 {
				t.Errorf("the fake GitHub saw %d requests, want 0", got)
			}
		})
	}
}

// The guard does not refuse what it must allow: an https GitHub Enterprise
// Server root and plain http on a loopback host (the fake) both pass.
func TestGitHubAcceptsHTTPSAndLoopbackAPIBase(t *testing.T) {
	for _, base := range []string{"https://ghe.example.com/api/v3", "https://api.github.com"} {
		if err := NewGitHubAdapter(msg.ChannelConfig{
			BotToken: "test-token-not-real", AppSecret: "s", APIBase: base, BotLogin: "example_bot",
			Channels: []string{"acme/widgets"}, PairingPolicy: "drop",
		}).checkConfig(); err != nil {
			t.Errorf("checkConfig refused %s: %v", base, err)
		}
	}
	h := newGitHubHarness(t, nil)
	if err := h.a.Validate(context.Background()); err != nil {
		t.Fatalf("Validate against the loopback fake: %v", err)
	}
}

// G-4: with no bot_login there is no handle at all — nothing triggers, and in
// particular not "@rysh", another person's GitHub account.
func TestGitHubHasNoDefaultTriggerHandle(t *testing.T) {
	a := NewGitHubAdapter(msg.ChannelConfig{})
	if a.botLogin != "" {
		t.Fatalf("default bot_login = %q, want none", a.botLogin)
	}
	for _, body := range []string{"@rysh fix it", "@ryshagent fix it"} {
		if a.mention != nil && a.mention.MatchString(body) {
			t.Errorf("%q triggers with no bot_login set", body)
		}
	}
}
