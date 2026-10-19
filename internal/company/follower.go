// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/upstream"
)

const (
	defaultPollEvery    = 60 * time.Second
	authFailedPollEvery = 5 * time.Minute
	refreshDebounce     = 750 * time.Millisecond
	activityCap         = 50
	// authFailuresBeforeBackoff: one refusal can be a deploy or a blip; two in
	// a row is a revoked key, and the view says so.
	authFailuresBeforeBackoff = 2
)

// Follower keeps one company's snapshot fresh: a REST fetch on start, on
// every live hint from the workspace NATS namespace, on demand (Refresh), and
// on a timer regardless. Every hint is only a reason to fetch; REST is the
// truth (design 037 §Architecture).
type Follower struct {
	client      *Client
	companyID   string
	workspaceID string
	upstream    config.UpstreamConfig
	pollEvery   time.Duration
	onSnapshot  func(msg.CompanySnapshot)

	mu           sync.Mutex
	snap         msg.CompanySnapshot
	activity     []msg.CompanyActivity
	authFailures int
	live         bool
	refresh      chan struct{}
}

// New builds a follower. onSnapshot is called on the follower's goroutine
// after every fetch and on every live/offline transition; it must not block.
func New(client *Client, companyID, workspaceID string, up config.UpstreamConfig, onSnapshot func(msg.CompanySnapshot)) *Follower {
	return &Follower{
		client: client, companyID: companyID, workspaceID: workspaceID, upstream: up,
		pollEvery: defaultPollEvery, onSnapshot: onSnapshot,
		refresh: make(chan struct{}, 1),
	}
}

// Snapshot returns the latest snapshot (a copy).
func (f *Follower) Snapshot() msg.CompanySnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

// Refresh asks for a fetch soon. Non-blocking; coalesces.
func (f *Follower) Refresh() {
	select {
	case f.refresh <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done.
func (f *Follower) Run(ctx context.Context) {
	f.fetch(ctx)
	go f.tail(ctx)
	timer := time.NewTimer(f.nextPoll())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.refresh:
			// Coalesce a burst of hints (an order and its payment arrive
			// within a second) into one fetch.
			debounce := time.NewTimer(refreshDebounce)
		drain:
			for {
				select {
				case <-ctx.Done():
					debounce.Stop()
					return
				case <-f.refresh:
				case <-debounce.C:
					break drain
				}
			}
			f.fetch(ctx)
		case <-timer.C:
			f.fetch(ctx)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(f.nextPoll())
	}
}

func (f *Follower) nextPoll() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return pollInterval(f.pollEvery, f.authFailures >= authFailuresBeforeBackoff)
}

// pollInterval is the poll cadence: the configured one, or a slow one once the
// key has been refused — a revoked key must not hammer the server.
func pollInterval(base time.Duration, authFailed bool) time.Duration {
	if base <= 0 {
		base = defaultPollEvery
	}
	if authFailed {
		return authFailedPollEvery
	}
	return base
}

func (f *Follower) fetch(ctx context.Context) {
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	snap, err := f.client.Fetch(fctx, f.companyID)
	cancel()

	f.mu.Lock()
	next := f.snap
	switch {
	case err == nil:
		f.authFailures = 0
		next = *snap
	case errors.Is(err, ErrUnauthorized):
		f.authFailures++
		next.Link.Connected = false
		next.Link.AuthFailed = f.authFailures >= authFailuresBeforeBackoff
		next.Link.LastError = err.Error()
	default:
		next.Link.Connected = false
		next.Link.LastError = err.Error()
	}
	next.Link.Live = f.live
	next.Activity = mergeActivity(f.activity, next.Activity)
	f.snap = next
	cb := f.onSnapshot
	f.mu.Unlock()
	if cb != nil {
		cb(next)
	}
}

// mergeActivity puts the live hints (newest first) ahead of the provisioning
// events the fetch replayed, capped.
func mergeActivity(live, fetched []msg.CompanyActivity) []msg.CompanyActivity {
	out := make([]msg.CompanyActivity, 0, len(live)+len(fetched))
	out = append(out, live...)
	for i := len(fetched) - 1; i >= 0; i-- {
		out = append(out, fetched[i])
	}
	if len(out) > activityCap {
		out = out[:activityCap]
	}
	return out
}

// pushActivity prepends one hint to a newest-first ring, capped.
func pushActivity(ring []msg.CompanyActivity, item msg.CompanyActivity, limit int) []msg.CompanyActivity {
	if item.OccurredAt.IsZero() {
		item.OccurredAt = time.Now().UTC()
	}
	ring = append([]msg.CompanyActivity{item}, ring...)
	if len(ring) > limit {
		ring = ring[:limit]
	}
	return ring
}

func (f *Follower) noteActivity(item msg.CompanyActivity) {
	f.mu.Lock()
	f.activity = pushActivity(f.activity, item, activityCap)
	f.mu.Unlock()
	f.Refresh()
}

func (f *Follower) setLive(live bool) {
	f.mu.Lock()
	changed := f.live != live
	f.live = live
	f.snap.Link.Live = live
	snap := f.snap
	cb := f.onSnapshot
	f.mu.Unlock()
	if changed && cb != nil {
		cb(snap)
	}
}

// ---------------------------------------------------------------------------
// Live tail over the server's NATS proxy
// ---------------------------------------------------------------------------

// ActivitySubject and ProgressSubject are the two subjects a connected client
// follows; both sit inside the workspace namespace the key is scoped to.
func ActivitySubject(workspaceID, companyID string) string {
	return "ws." + workspaceID + ".ai-native-company." + companyID + ".activity"
}

func ProgressSubject(workspaceID, companyID string) string {
	return "ws." + workspaceID + ".ai-native-company." + companyID + ".*.progress"
}

// NATSWebSocketURL turns the server's HTTP origin into the WebSocket origin
// the proxy speaks on.
func NATSWebSocketURL(serverURL string) string {
	u := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// ProxyPath is the upgrade path the proxy scopes by workspace. The API key is
// NOT in it: it rides in the X-API-Key upgrade header
// (upstream.NATSAPIKeyHeader), because a URL lands in access logs (F-86).
func ProxyPath(workspaceID string) string {
	return "/workspaces/" + workspaceID + "/nats?connection_type=share"
}

func (f *Follower) tail(ctx context.Context) {
	url := f.upstream.URL
	if url == "" {
		url = f.client.BaseURL
	}
	if url == "" || f.workspaceID == "" {
		return
	}
	opts := []nats.Option{
		nats.Name("rysh-company-" + shortID(f.companyID)),
		nats.ProxyPath(ProxyPath(f.workspaceID)),
		upstream.NATSAPIKeyHeader(f.client.APIKey), // the key rides in a header, not the URL (F-86)
		nats.Token(f.client.APIKey),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(3 * time.Second),
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, _ error) { f.setLive(false) }),
		nats.ReconnectHandler(func(_ *nats.Conn) { f.setLive(true); f.Refresh() }),
	}
	nc, err := nats.Connect(NATSWebSocketURL(url), opts...)
	if err != nil {
		return
	}
	defer nc.Close()
	if nc.IsConnected() {
		f.setLive(true)
	}
	_, _ = nc.Subscribe(ActivitySubject(f.workspaceID, f.companyID), func(m *nats.Msg) {
		var item msg.CompanyActivity
		if json.Unmarshal(m.Data, &item) != nil {
			return
		}
		f.noteActivity(item)
	})
	_, _ = nc.Subscribe(ProgressSubject(f.workspaceID, f.companyID), func(m *nats.Msg) {
		var ev progressEvent
		if json.Unmarshal(m.Data, &ev) != nil {
			return
		}
		summary := ev.Message
		if summary == "" {
			summary = ev.Stage
		}
		if ev.ServiceKey != "" {
			summary = ev.ServiceKey + ": " + summary
		}
		f.noteActivity(msg.CompanyActivity{Kind: "progress", Action: ev.Stage, Status: ev.Status, Summary: summary, OccurredAt: ev.OccurredAt})
	})
	<-ctx.Done()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// String describes the link for logs and ##company status.
func (l LinkDescription) String() string { return string(l) }

// LinkDescription is a one-line reading of a CompanyLink.
type LinkDescription string

func DescribeLink(l msg.CompanyLink) LinkDescription {
	switch {
	case l.AuthFailed:
		return "key refused — reconnect from the dashboard"
	case !l.Connected && l.LastError != "":
		return LinkDescription("disconnected: " + l.LastError)
	case !l.Connected:
		return "connecting…"
	case l.Live:
		return "live"
	}
	return LinkDescription(fmt.Sprintf("connected (polling every %s)", defaultPollEvery))
}
