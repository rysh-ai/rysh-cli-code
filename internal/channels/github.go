// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// defaultGitHubWebhookPort is the loopback port the webhook listener binds
	// when webhook_port is not set in the skill file.
	defaultGitHubWebhookPort = 23314
	// defaultGitHubAPIBase is the public GitHub REST API; api_base overrides it
	// (GitHub Enterprise Server, and every test's fake GitHub).
	defaultGitHubAPIBase = "https://api.github.com"
	// githubAPIVersion pins the REST API version the adapter was written for.
	githubAPIVersion = "2022-11-28"
	// githubMaxBodyLen keeps each reply under GitHub's 65,536-character
	// comment limit, with headroom; longer replies become several comments.
	githubMaxBodyLen = 65000
	// githubMaxPayload bounds a webhook body read (GitHub caps payloads at
	// 25 MB; the events this channel reads are a few KB).
	githubMaxPayload = 5 << 20
	// githubHandledCap bounds the set of deliveries and comments already
	// handled, used to answer each request once.
	githubHandledCap = 1000
)

// githubTrustedAssociations are the author_association values whose mentions
// are acted on. Anyone else — including a CONTRIBUTOR or a stranger on a
// public repository — can write "@<bot_login>", so their comments are dropped before
// they ever become a prompt. The pairing gate/allowlist still applies on top.
var githubTrustedAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// githubRepoPattern is an "owner/name" repository, as GitHub spells it.
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GitHubAdapter implements ChannelAdapter for GitHub issues and pull requests:
// a comment that mentions the bot ("@<bot_login> …") on a listed repository becomes
// an inbound message, and the reply is posted as a comment on the same issue
// or pull request (gap04, G16).
//
// Inbound: GitHub delivers webhooks (a repository webhook today; the GitHub
// App's later) to a public HTTPS URL, so the adapter runs a loopback-only HTTP
// listener on WebhookPort that the operator's tunnel forwards to. Every
// delivery's X-Hub-Signature-256 is verified against the webhook secret
// (AppSecret); there is no unsigned mode.
//
// Outbound: a direct REST call, POST /repos/{owner}/{name}/issues/{n}/comments,
// with BotToken. A pull request is an issue to this endpoint, so PR replies
// land on the PR conversation.
type GitHubAdapter struct {
	config   msg.ChannelConfig
	inbound  chan InboundMessage
	client   *http.Client
	apiBase  string
	botLogin string
	mention  *regexp.Regexp
	repos    map[string]bool // lowercased "owner/name"
	port     int

	mu        sync.RWMutex
	connected bool
	// relayWork tracks the relay's background replays so Stop can wait for
	// them; relayStopping (under mu) stops new ones from starting once Stop
	// has begun. See goRelay.
	relayWork     sync.WaitGroup
	relayStopping bool
	err           string
	server        *http.Server
	ln            net.Listener
	cancel        context.CancelFunc

	// tokenLogin is the account bot_token belongs to, resolved at Start. It
	// is "self" alongside botLogin: when replies are posted from an ordinary
	// account, its own approval notice mentions the bot.
	tokenLogin string

	// handled remembers delivery ids and comment ids already turned into a
	// prompt, so a webhook GitHub retries — or one an operator redelivers,
	// which carries a new delivery id — is answered once.
	handled      map[string]struct{}
	handledOrder []string

	// Relay mode (github_relay.go): the connection to rysh-server, the newest
	// inbound timestamp handled (replay watermark), and whether another rysh
	// session holds this connection (D-21).
	relayConn     *nats.Conn
	relayLastSeen int64
	relayRefused  bool
	relayRefusals int
}

// NewGitHubAdapter creates a GitHub adapter. Configuration problems are
// reported by Start, not here, so the factory never fails on a half-filled
// skill file.
func NewGitHubAdapter(config msg.ChannelConfig) *GitHubAdapter {
	port := defaultGitHubWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	apiBase := strings.TrimRight(strings.TrimSpace(config.APIBase), "/")
	if apiBase == "" {
		apiBase = defaultGitHubAPIBase
	}
	// No default handle (G-4): "rysh" is someone else's GitHub account and
	// "ryshagent" is unowned, so a default would notify a stranger. Start
	// refuses without bot_login; until then nothing triggers.
	login := strings.TrimPrefix(strings.TrimSpace(config.BotLogin), "@")
	var mention *regexp.Regexp
	if login != "" && !strings.Contains(login, "${") {
		// A whole handle only: for bot_login "example_bot", "@example_bot-ci",
		// "ops@example_bot.dev" and "@example_bot/tools" do not trigger;
		// "(@example_bot)" and "@example_bot," do.
		// Logins are case-insensitive on GitHub.
		mention = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.@/-])@` + regexp.QuoteMeta(login) + `($|[^A-Za-z0-9_/-])`)
	}
	repos := make(map[string]bool, len(config.Channels))
	for _, r := range config.Channels {
		if r = strings.TrimSpace(r); r != "" {
			repos[strings.ToLower(r)] = true
		}
	}
	g := &GitHubAdapter{
		config:   config,
		inbound:  make(chan InboundMessage, 100),
		client:   &http.Client{Timeout: 20 * time.Second},
		apiBase:  apiBase,
		botLogin: login,
		mention:  mention,
		repos:    repos,
		port:     port,
		handled:  make(map[string]struct{}),
	}
	if config.Relay {
		// As WhatsApp loads its store at construction: what this directory
		// already answered is known before the first delivery arrives.
		g.loadRelayState()
	}
	return g
}

// Type returns "github".
func (g *GitHubAdapter) Type() string { return "github" }

// checkConfig is the fail-closed gate Start applies before binding anything.
func (g *GitHubAdapter) checkConfig() error {
	if g.config.Relay {
		return g.checkRelayConfig()
	}
	if strings.TrimSpace(g.config.AppSecret) == "" || strings.Contains(g.config.AppSecret, "${") {
		return fmt.Errorf("github: app_secret (the webhook secret) is required — every delivery's signature is verified, and an unset ${…} reference counts as missing")
	}
	if strings.TrimSpace(g.config.BotToken) == "" {
		return fmt.Errorf("github: bot_token is required (replies are posted with it)")
	}
	if err := unresolvedTokenRef("github", g.config.BotToken); err != nil {
		return err
	}
	if err := ValidateAPIBase(g.config.APIBase); err != nil {
		return fmt.Errorf("github: %w", err)
	}
	return g.checkTriggerConfig()
}

// checkTriggerConfig is the part of the gate both modes share: who triggers,
// where, and that pairing is never offered on a public thread.
func (g *GitHubAdapter) checkTriggerConfig() error {
	if g.mention == nil {
		return fmt.Errorf("github: bot_login is required — the handle that triggers the channel (your App or bot account); there is no default, and an unset ${…} reference counts as missing")
	}
	if len(g.repos) == 0 {
		return fmt.Errorf("github: channels must list the owner/name repositories to answer in; with none it would answer anywhere its webhook is installed")
	}
	for r := range g.repos {
		if !githubRepoPattern.MatchString(r) {
			return fmt.Errorf("github: channels entry %q is not an owner/name repository", r)
		}
	}
	// A pairing request would ask a stranger for a code on a public thread,
	// and "open" admits every trusted commenter ungated. Only drop, or a
	// declared allowlist, may start.
	if !strings.EqualFold(strings.TrimSpace(g.config.PairingPolicy), "drop") && !hasAllowlistEntry(g.config.Allowlist) {
		return fmt.Errorf("github: set pairing_policy: drop or an allowlist of GitHub logins; pairing is not offered on a public thread")
	}
	return nil
}

func hasAllowlistEntry(list []string) bool {
	for _, e := range list {
		if strings.TrimSpace(e) != "" {
			return true
		}
	}
	return false
}

// Start validates the configuration and launches the loopback webhook listener,
// or in relay mode dials rysh-server instead (github_relay.go).
func (g *GitHubAdapter) Start(ctx context.Context) error {
	if err := g.checkConfig(); err != nil {
		g.mu.Lock()
		g.err = err.Error()
		g.mu.Unlock()
		return err
	}
	if g.config.Relay {
		if err := g.startRelay(ctx); err != nil {
			g.mu.Lock()
			g.err = err.Error()
			g.connected = false
			g.mu.Unlock()
			return err
		}
		return nil
	}

	// Know who "self" is before reading any delivery. A token GitHub rejects
	// could not post a reply either, so it stops Start.
	idCtx, idCancel := context.WithTimeout(ctx, 20*time.Second)
	id, err := g.ValidateIdentity(idCtx)
	idCancel()
	if err != nil {
		g.mu.Lock()
		g.err = err.Error()
		g.mu.Unlock()
		return fmt.Errorf("github: resolving the bot_token account: %w", err)
	}
	g.mu.Lock()
	g.tokenLogin = id.Username
	g.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel

	// Loopback only: the tunnel the operator runs is the one way in, so the
	// listener is never reachable from the LAN directly.
	addr := fmt.Sprintf("127.0.0.1:%d", g.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		g.mu.Lock()
		g.err = err.Error()
		g.mu.Unlock()
		return fmt.Errorf("github: webhook listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	// Mounted at the root so the tunnel may map any external path to it.
	mux.HandleFunc("/", g.handleWebhook)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	g.mu.Lock()
	g.server = server
	g.ln = ln
	g.connected = true
	g.err = ""
	g.mu.Unlock()

	go func() {
		if serr := server.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			slog.Error("github: webhook server stopped", "err", serr)
			g.mu.Lock()
			g.err = serr.Error()
			g.connected = false
			g.mu.Unlock()
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = server.Shutdown(shutCtx)
		g.mu.Lock()
		g.connected = false
		g.mu.Unlock()
		slog.Info("github: adapter stopped")
	}()

	slog.Info("github: adapter started",
		"webhook", addr, "repos", g.config.Channels, "trigger", "@"+g.botLogin, "api_base", g.apiBase)
	return nil
}

// listenAddr reports where the webhook listener is bound ("" before Start).
func (g *GitHubAdapter) listenAddr() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.ln == nil {
		return ""
	}
	return g.ln.Addr().String()
}

// Stop shuts the webhook listener down.
func (g *GitHubAdapter) Stop() error {
	g.mu.Lock()
	g.relayStopping = true
	g.mu.Unlock()
	if g.cancel != nil {
		g.cancel()
	}
	g.mu.Lock()
	g.connected = false
	g.mu.Unlock()
	// A replay in flight ends when its request does: the cancel above closes
	// the relay connection, and the request is bounded by relayReplyTimeout.
	g.relayWork.Wait()
	return nil
}

// InboundCh returns the channel of inbound @mentions.
func (g *GitHubAdapter) InboundCh() <-chan InboundMessage { return g.inbound }

// SetReplyMode is a no-op: on GitHub the channel only ever answers mentions.
func (g *GitHubAdapter) SetReplyMode(_ string) {}

// Status returns the current GitHub channel status.
func (g *GitHubAdapter) Status() msg.ChannelStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()
	where := fmt.Sprintf("webhook 127.0.0.1:%d", g.port)
	if g.config.Relay {
		where = "relay " + g.relaySubject("inbound")
	}
	return msg.ChannelStatus{
		Type:      "github",
		Connected: g.connected,
		Error:     g.err,
		Details: fmt.Sprintf("%s, trigger @%s, repos: %s",
			where, g.botLogin, strings.Join(g.config.Channels, ", ")),
	}
}

// verifyGitHubSignature checks X-Hub-Signature-256 ("sha256=<hex>") over the
// raw body. An empty secret never verifies.
func verifyGitHubSignature(body []byte, header, secret string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// handleWebhook verifies and dispatches one GitHub delivery. Signed deliveries
// it does not act on still get a 2xx, so GitHub does not retry them.
func (g *GitHubAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, githubMaxPayload))
	if err != nil {
		http.Error(rw, "read error", http.StatusBadRequest)
		return
	}
	if !verifyGitHubSignature(body, r.Header.Get("X-Hub-Signature-256"), g.config.AppSecret) {
		slog.Warn("github: webhook signature missing or wrong; delivery refused")
		http.Error(rw, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	switch event {
	case "ping":
		rw.WriteHeader(http.StatusOK)
		slog.Info("github: webhook ping received", "delivery", delivery)
		return
	case "issue_comment", "issues", "pull_request_review_comment":
	default:
		rw.WriteHeader(http.StatusAccepted)
		slog.Debug("github: ignoring event", "event", event)
		return
	}

	t, ok := parseGitHubTrigger(event, body)
	rw.WriteHeader(http.StatusOK)
	if !ok {
		return
	}
	if reason := g.refuse(t); reason != "" {
		slog.Debug("github: not acting on delivery", "event", event, "repo", t.repo,
			"number", t.number, "author", t.author.Login, "reason", reason)
		return
	}
	if !g.markHandled(delivery, t.dedupeKey) {
		slog.Debug("github: ignoring repeated delivery", "delivery", delivery, "key", t.dedupeKey)
		return
	}

	im := g.inboundFor(t, delivery)
	select {
	case g.inbound <- im:
	default:
		slog.Warn("github: inbound buffer full, dropping mention", "thread", im.ThreadID)
		return
	}
	// Tell the commenter it was picked up. Async: the webhook answer must not
	// wait on a second API round trip.
	go g.react(t)
}

// githubTrigger is one webhook event normalised to what the adapter needs.
type githubTrigger struct {
	event       string
	action      string
	repo        string
	number      int
	kind        string // "issue" or "pull_request"
	url         string
	author      githubUser
	sender      githubUser
	association string
	text        string
	commentID   int64
	reactPath   string // REST path that takes a reaction on the trigger
	dedupeKey   string
}

type githubUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubEvent struct {
	Action string `json:"action"`
	Issue  *struct {
		Number            int             `json:"number"`
		Body              string          `json:"body"`
		HTMLURL           string          `json:"html_url"`
		PullRequest       json.RawMessage `json:"pull_request"`
		User              githubUser      `json:"user"`
		AuthorAssociation string          `json:"author_association"`
	} `json:"issue"`
	Comment *struct {
		ID                int64      `json:"id"`
		Body              string     `json:"body"`
		HTMLURL           string     `json:"html_url"`
		User              githubUser `json:"user"`
		AuthorAssociation string     `json:"author_association"`
	} `json:"comment"`
	PullRequest *struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender githubUser `json:"sender"`
}

// parseGitHubTrigger normalises the three events the channel reads. It
// reports false for a payload missing the parts the event must carry.
func parseGitHubTrigger(event string, body []byte) (githubTrigger, bool) {
	var ev githubEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		slog.Warn("github: unparseable webhook payload", "event", event, "err", err)
		return githubTrigger{}, false
	}
	t := githubTrigger{event: event, action: ev.Action, repo: ev.Repository.FullName, sender: ev.Sender}
	switch event {
	case "issue_comment":
		if ev.Issue == nil || ev.Comment == nil {
			return t, false
		}
		t.number, t.url = ev.Issue.Number, ev.Issue.HTMLURL
		t.kind = "issue"
		if len(ev.Issue.PullRequest) > 0 && string(ev.Issue.PullRequest) != "null" {
			t.kind = "pull_request"
		}
		t.author, t.association, t.text = ev.Comment.User, ev.Comment.AuthorAssociation, ev.Comment.Body
		t.commentID = ev.Comment.ID
		t.reactPath = fmt.Sprintf("/repos/%s/issues/comments/%d/reactions", t.repo, t.commentID)
		t.dedupeKey = fmt.Sprintf("issue_comment:%d", t.commentID)
	case "pull_request_review_comment":
		if ev.PullRequest == nil || ev.Comment == nil {
			return t, false
		}
		t.number, t.url = ev.PullRequest.Number, ev.PullRequest.HTMLURL
		t.kind = "pull_request"
		t.author, t.association, t.text = ev.Comment.User, ev.Comment.AuthorAssociation, ev.Comment.Body
		t.commentID = ev.Comment.ID
		t.reactPath = fmt.Sprintf("/repos/%s/pulls/comments/%d/reactions", t.repo, t.commentID)
		t.dedupeKey = fmt.Sprintf("review_comment:%d", t.commentID)
	case "issues":
		if ev.Issue == nil {
			return t, false
		}
		t.number, t.url = ev.Issue.Number, ev.Issue.HTMLURL
		t.kind = "issue"
		t.author, t.association, t.text = ev.Issue.User, ev.Issue.AuthorAssociation, ev.Issue.Body
		t.reactPath = fmt.Sprintf("/repos/%s/issues/%d/reactions", t.repo, t.number)
		t.dedupeKey = fmt.Sprintf("issue:%s#%d", strings.ToLower(t.repo), t.number)
	default:
		return t, false
	}
	if t.number <= 0 || t.repo == "" {
		return t, false
	}
	return t, true
}

// refuse returns why a parsed trigger must not become a prompt, or "".
func (g *GitHubAdapter) refuse(t githubTrigger) string {
	// A new comment, or a new issue: an edit that adds "@<bot_login>" later is not a
	// request, and answering it would answer the same thread twice.
	wantAction := "created"
	if t.event == "issues" {
		wantAction = "opened"
	}
	switch {
	case t.action != wantAction:
		return "action " + t.action
	case !g.repos[strings.ToLower(t.repo)]:
		return "repository not listed"
	case g.isSelf(t.author) || g.isSelf(t.sender):
		return "own comment"
	case strings.EqualFold(t.author.Type, "Bot") || strings.EqualFold(t.sender.Type, "Bot"):
		return "bot author"
	case !githubTrustedAssociations[t.association]:
		return "author_association " + t.association
	case g.mention == nil || !g.mention.MatchString(t.text):
		return "no mention"
	}
	return ""
}

// isSelf reports whether u is the bot's own account: the trigger handle (a
// user or its App bot) or the account bot_token posts as.
func (g *GitHubAdapter) isSelf(u githubUser) bool {
	g.mu.RLock()
	token := g.tokenLogin
	g.mu.RUnlock()
	return strings.EqualFold(u.Login, g.botLogin) || strings.EqualFold(u.Login, g.botLogin+"[bot]") ||
		(token != "" && strings.EqualFold(u.Login, token))
}

// markHandled records the delivery and the request it carries, reporting
// false when either was already handled.
func (g *GitHubAdapter) markHandled(keys ...string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, seen := g.handled[k]; seen {
			return false
		}
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		g.handled[k] = struct{}{}
		g.handledOrder = append(g.handledOrder, k)
	}
	for len(g.handledOrder) > githubHandledCap {
		delete(g.handled, g.handledOrder[0])
		g.handledOrder = g.handledOrder[1:]
	}
	return true
}

// inboundFor builds the inbound message. Content is the comment with the
// mention removed and nothing else, so "@<bot_login> yes" arrives as exactly "yes" —
// the humanoid matches approval replies on the whole text. Everything about
// where it was said rides in Metadata, which the humanoid renders into the
// prompt. The title, branch name and file path are left out: whoever opened
// the issue or PR wrote them, and that may be a stranger.
func (g *GitHubAdapter) inboundFor(t githubTrigger, delivery string) InboundMessage {
	md := map[string]string{
		"repo":     t.repo,
		"number":   strconv.Itoa(t.number),
		"kind":     t.kind,
		"url":      t.url,
		"delivery": delivery,
	}
	if t.commentID != 0 {
		md["comment_id"] = strconv.FormatInt(t.commentID, 10)
	}
	return InboundMessage{
		SenderID:   t.author.Login,
		SenderName: t.author.Login,
		Content:    strings.TrimSpace(g.mention.ReplaceAllString(t.text, "$1$2")),
		ThreadID:   fmt.Sprintf("%s#%d", t.repo, t.number),
		Metadata:   md,
	}
}

// react puts one 👀 reaction on the triggering comment or issue.
func (g *GitHubAdapter) react(t githubTrigger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := g.api(ctx, http.MethodPost, t.reactPath, map[string]string{"content": "eyes"}); err != nil {
		slog.Warn("github: reaction failed", "path", t.reactPath, "err", err)
	}
}

// parseGitHubThread splits "owner/name#n".
func parseGitHubThread(thread string) (repo string, number int, err error) {
	i := strings.LastIndex(thread, "#")
	if i <= 0 {
		return "", 0, fmt.Errorf("github: thread %q is not owner/name#number", thread)
	}
	repo = thread[:i]
	number, err = strconv.Atoi(thread[i+1:])
	if err != nil || number <= 0 || !githubRepoPattern.MatchString(repo) {
		return "", 0, fmt.Errorf("github: thread %q is not owner/name#number", thread)
	}
	return repo, number, nil
}

// Send posts the reply as a comment on the thread's issue or pull request.
// Progress steps are not posted: the commenter gets the 👀 reaction on pickup
// and one answer, not a comment per step. Outbound is a plain REST call, so it
// does not depend on the inbound listener being up.
func (g *GitHubAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	if outbound.Kind == OutboundKindStep {
		return nil
	}
	repo, number, err := parseGitHubThread(outbound.ThreadID)
	if err != nil {
		return err
	}
	if !g.repos[strings.ToLower(repo)] {
		return fmt.Errorf("github: repository %s is not in this channel's list", repo)
	}
	content := outbound.Content
	// The humanoid's approval notice says 'reply "yes"', but on GitHub only a
	// comment that mentions the bot reaches the session.
	if strings.HasPrefix(content, "[approval required]") {
		content += fmt.Sprintf("\n\nOn GitHub, answer with a mention: `@%s yes` or `@%s no`.", g.botLogin, g.botLogin)
	}
	if g.config.Relay {
		return g.sendRelayComment(ctx, repo, number, content)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	for _, chunk := range splitMessage(content, githubMaxBodyLen) {
		if _, err := g.api(ctx, http.MethodPost, path, map[string]string{"body": chunk}); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the token with one GET /user, binding nothing.
func (g *GitHubAdapter) Validate(ctx context.Context) error {
	_, err := g.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity reports which account the token belongs to. Relay mode
// holds no token: the App posts as <bot_login>[bot], which needs no call.
func (g *GitHubAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if g.config.Relay {
		if g.botLogin == "" {
			return BotIdentity{}, fmt.Errorf("github: bot_login is required")
		}
		return BotIdentity{Username: g.botLogin + "[bot]"}, nil
	}
	if strings.TrimSpace(g.config.BotToken) == "" {
		return BotIdentity{}, fmt.Errorf("github: bot_token is required")
	}
	if err := unresolvedTokenRef("github", g.config.BotToken); err != nil {
		return BotIdentity{}, err
	}
	raw, err := g.api(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return BotIdentity{}, err
	}
	var me struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := json.Unmarshal(raw, &me); err != nil || me.Login == "" {
		return BotIdentity{}, fmt.Errorf("github: GET /user returned no login")
	}
	return BotIdentity{ID: strconv.FormatInt(me.ID, 10), Username: me.Login}, nil
}

// api makes one authenticated REST call and returns the response body. A
// non-2xx status is an error carrying GitHub's own message.
func (g *GitHubAdapter) api(ctx context.Context, method, path string, payload any) ([]byte, error) {
	// The token rides in every call, so a bad api_base is refused before one
	// is built — Validate and Send reach here without Start's checkConfig.
	if err := ValidateAPIBase(g.config.APIBase); err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("github: encode request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase+path, body)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.config.BotToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "rysh")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		return nil, fmt.Errorf("github: %s %s: %d %s: %s", method, path, resp.StatusCode,
			http.StatusText(resp.StatusCode), apiErr.Message)
	}
	return raw, nil
}
