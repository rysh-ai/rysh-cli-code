// SPDX-License-Identifier: Apache-2.0

package actors

// gap04 (root-order-074, G16): an @example_bot mention on a GitHub issue or PR runs
// through the same humanoid path as every other channel, approvals included.
// The proof drives the real github adapter end to end — a signed webhook over
// its loopback listener, the humanoid's own inboundLoop, the approval hold,
// the notice posted back as a comment — against a local fake GitHub. No real
// App, token or network.

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

type gap04Comment struct {
	Path string
	Body string
}

// gap04FakeGitHub records the comments and reactions the adapter posts.
func gap04FakeGitHub(t *testing.T) (*httptest.Server, func() []gap04Comment) {
	t.Helper()
	var mu sync.Mutex
	var got []gap04Comment
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		if r.Method == http.MethodGet && r.URL.Path == "/user" {
			// Start resolves the reply token's account (it is "self").
			_, _ = io.WriteString(w, `{"login":"acme-ops","id":5,"type":"User"}`)
			return
		}
		mu.Lock()
		got = append(got, gap04Comment{Path: r.URL.Path, Body: body["body"]})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []gap04Comment {
		mu.Lock()
		defer mu.Unlock()
		return append([]gap04Comment(nil), got...)
	}
}

func gap04FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// gap04Comment builds a synthetic issue_comment "created" payload (the
// schema of internal/channels/testdata/github/issue_comment_created.json).
func gap04CommentPayload(login, association string, commentID int, body string) []byte {
	p := map[string]any{
		"action": "created",
		"issue": map[string]any{
			"number": 42, "title": "Parser drops the last token",
			"html_url": "https://github.com/acme/widgets/issues/42",
			"user":     map[string]any{"login": "alice", "type": "User"},
		},
		"comment": map[string]any{
			"id": commentID, "body": body,
			"user":               map[string]any{"login": login, "type": "User"},
			"author_association": association,
		},
		"repository": map[string]any{"full_name": "acme/widgets"},
		"sender":     map[string]any{"login": login, "type": "User"},
	}
	out, _ := json.Marshal(p)
	return out
}

func TestGitHubMentionApprovalRoundTrip(t *testing.T) {
	h, _, drainResp := newAssistantForTest(t, "owner-1")
	no := false
	h.autoApprove = &no

	secretBytes := make([]byte, 16)
	if _, err := rand.Read(secretBytes); err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(secretBytes)
	fake, comments := gap04FakeGitHub(t)
	port := gap04FreePort(t)
	cc := msg.ChannelConfig{
		Enabled:       true,
		BotToken:      "test-token-not-real",
		AppSecret:     secret,
		WebhookPort:   strconv.Itoa(port),
		Channels:      []string{"acme/widgets"},
		APIBase:       fake.URL,
		PairingPolicy: "drop",
		Allowlist:     []string{"alice"},
		// A placeholder handle: "_" is not allowed in a GitHub login, so it can
		// never be a real account (G-4: bot_login is required, no default).
		BotLogin: "example_bot",
	}
	adapter, err := channels.NewAdapter("github", cc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer adapter.Stop()
	h.contacts["github"] = cc
	h.adapters["github"] = adapter
	h.mergeDeclaredAllowlists()

	// The humanoid's own inbound loop forwards adapter messages to its inbox.
	inbox, err := h.nc.SubscribeSync(msg.T("humanoid", h.name, "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	go h.inboundLoop(ctx, "github", adapter)

	deliver := func(n int, payload []byte) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("X-GitHub-Delivery", "gap04-"+strconv.Itoa(n))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("webhook %d = %d", n, resp.StatusCode)
		}
	}
	next := func() *msg.MsgHumanoidInboundMessage {
		t.Helper()
		envs := drainSubject(t, inbox, 500*time.Millisecond)
		if len(envs) != 1 {
			t.Fatalf("want 1 inbound on the humanoid inbox, got %d", len(envs))
		}
		var m msg.MsgHumanoidInboundMessage
		if err := json.Unmarshal(envs[0].Payload, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}

	// 1. alice asks; the run raises a tool approval, which is HELD.
	deliver(1, gap04CommentPayload("alice", "OWNER", 1001, "@example_bot run the parser tests"))
	first := next()
	if first.ChannelType != "github" || first.SenderID != "alice" || first.ThreadID != "acme/widgets#42" || first.Content != "run the parser tests" {
		t.Fatalf("inbound = %+v", first)
	}
	h.lastInbound = first
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-gh", Description: "bash: go test ./parser/..."})
	if _, ok := h.pendingApprovals["req-gh"]; !ok {
		t.Fatal("GitHub-triggered tool call not held for approval")
	}
	if got := drainResp(300 * time.Millisecond); len(got) != 0 {
		t.Fatalf("GitHub-triggered tool call auto-approved (%d responses)", len(got))
	}

	// 2. The notice is posted on the issue, telling alice how to answer.
	var notice *gap04Comment
	deadline := time.Now().Add(2 * time.Second)
	for notice == nil && time.Now().Before(deadline) {
		for _, c := range comments() {
			if c.Path == "/repos/acme/widgets/issues/42/comments" {
				c := c
				notice = &c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if notice == nil {
		t.Fatalf("approval notice never posted on the issue; GitHub saw %+v", comments())
	}
	if !strings.Contains(notice.Body, "[approval required]") || !strings.Contains(notice.Body, "go test ./parser/...") || !strings.Contains(notice.Body, "@example_bot yes") {
		t.Fatalf("notice = %q", notice.Body)
	}

	// 2b. GitHub delivers the notice back: it was posted by the reply token's
	// account (acme-ops, an OWNER) and its footer says "@example_bot yes". It must
	// not reach the session, or the bot would answer itself.
	deliver(10, gap04CommentPayload("acme-ops", "OWNER", 1010, notice.Body))
	if envs := drainSubject(t, inbox, 300*time.Millisecond); len(envs) != 0 {
		t.Fatalf("the reply account's own notice reached the humanoid inbox (%d messages)", len(envs))
	}
	if _, ok := h.pendingApprovals["req-gh"]; !ok {
		t.Fatal("the bot's own notice cleared the approval")
	}

	// 3. A collaborator who is not on the allowlist answers "@example_bot yes": the
	// pairing gate drops it and the approval stays pending.
	deliver(2, gap04CommentPayload("mallory", "COLLABORATOR", 1002, "@example_bot yes"))
	h.handleInboundMessage(next())
	if _, ok := h.pendingApprovals["req-gh"]; !ok {
		t.Fatal("a non-allowlisted collaborator's yes cleared the approval")
	}
	if got := drainResp(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("a non-allowlisted collaborator's yes released the approval (%d responses)", len(got))
	}

	// 4. alice's "@example_bot yes" on the same issue is the decision.
	deliver(3, gap04CommentPayload("alice", "OWNER", 1003, "@example_bot yes"))
	h.handleInboundMessage(next())
	envs := drainResp(time.Second)
	if len(envs) != 1 {
		t.Fatalf("want one approval response, got %d", len(envs))
	}
	var resp msg.MsgApprovalResponse
	if err := json.Unmarshal(envs[0].Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "req-gh" || resp.Decision != msg.DecisionYes {
		t.Fatalf("response = %+v, want req-gh approved", resp)
	}
}

func TestGitHubPromptCarriesIssueContext(t *testing.T) {
	h := &HumanoidActor{name: "assistant"}
	m := &msg.MsgHumanoidInboundMessage{
		ChannelType: "github",
		SenderID:    "alice",
		SenderName:  "alice",
		ThreadID:    "acme/widgets#7",
		Content:     "is this off by one?",
		Metadata: map[string]string{
			"repo": "acme/widgets", "number": "7", "kind": "pull_request",
			"title": "Add retry to the uploader", "url": "https://github.com/acme/widgets/pull/7",
			"path": "uploader/retry.go", "line": "31", "head_ref": "retry-uploader",
		},
	}
	conv := &ConversationContext{History: []ConversationTurn{{Role: "user", Content: m.Content}}}
	p := h.buildContextualPrompt(m, conv)
	for _, want := range []string{
		"GitHub", "@alice", "acme/widgets#7", "pull request",
		"https://github.com/acme/widgets/pull/7",
		"is this off by one?", "comment on",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// R7: the title, branch and file path are written by whoever opened the
	// issue or PR — possibly a stranger — so they never reach the prompt,
	// even when present in Metadata.
	for _, banned := range []string{"Add retry to the uploader", "uploader/retry.go", "retry-uploader"} {
		if strings.Contains(p, banned) {
			t.Errorf("prompt carries stranger-written text %q:\n%s", banned, p)
		}
	}
}
