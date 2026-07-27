package channels

// Outbound half of the Twilio SMS adapter.
//
// This file began life as the X3 regression guard: PhoneAdapter.Send used to
// log "phone: send (stub)" and return nil, so every caller believed an SMS had
// been delivered while nothing left the process. The stub is gone and the
// transport is real, but the invariant it protected is the reason these tests
// exist — Send reports success only when Twilio accepted the message, and says
// plainly that nothing was sent when it did not.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeTwilio is a stand-in Messages.json endpoint that records what it was
// asked to send. No test in this package talks to the real API.
type fakeTwilio struct {
	mu       sync.Mutex
	requests []url.Values
	auth     []string
	// respond returns the status and body for the nth request (1-based).
	respond func(n int) (int, string)
}

func newFakeTwilio(t *testing.T, respond func(n int) (int, string)) (*fakeTwilio, *httptest.Server) {
	t.Helper()
	f := &fakeTwilio{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("send request was not form-encoded: %v", err)
		}
		sid, tok, _ := r.BasicAuth()
		f.mu.Lock()
		f.requests = append(f.requests, r.PostForm)
		f.auth = append(f.auth, sid+":"+tok)
		n := len(f.requests)
		f.mu.Unlock()

		status, body := http.StatusCreated, `{"sid":"SMsent","status":"queued"}`
		if f.respond != nil {
			status, body = f.respond(n)
		}
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeTwilio) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeTwilio) request(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// connectedPhone returns a started-looking adapter pointed at the fake API.
// Send() refuses when not connected, and binding a real listener is not what
// these tests are about.
func connectedPhone(srvURL string, cfg msg.ChannelConfig) *PhoneAdapter {
	p := NewPhoneAdapter(cfg)
	p.baseURL = srvURL
	p.retryBaseDelay = time.Millisecond
	p.connected = true
	return p
}

func TestPhoneSendPostsToTwilio(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, nil)
	p := connectedPhone(srv.URL, phoneCfg())

	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: "+15550142", Content: "on my way",
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if fake.count() != 1 {
		t.Fatalf("posted %d messages, want 1", fake.count())
	}
	req := fake.request(0)
	if req.Get("To") != "+15550142" {
		t.Errorf("To = %q", req.Get("To"))
	}
	if req.Get("From") != "+15550100" {
		t.Errorf("From = %q, want the configured Twilio number", req.Get("From"))
	}
	if req.Get("Body") != "on my way" {
		t.Errorf("Body = %q", req.Get("Body"))
	}
	if fake.auth[0] != "ACtest:tok-test" {
		t.Errorf("basic auth = %q, want the account sid and auth token", fake.auth[0])
	}
}

// The X3 invariant: a message Twilio refused must never be reported as sent.
func TestPhoneSendReportsFailureInsteadOfFakingSuccess(t *testing.T) {
	t.Chdir(t.TempDir())
	_, srv := newFakeTwilio(t, func(int) (int, string) {
		return http.StatusBadRequest,
			`{"code":21610,"message":"The message From/To pair violates a blacklist rule."}`
	})
	p := connectedPhone(srv.URL, phoneCfg())

	err := p.Send(context.Background(), OutboundMessage{RecipientID: "+15550142", Content: "hello"})
	if err == nil {
		t.Fatal("Send returned nil for a message Twilio rejected")
	}
	if !strings.Contains(err.Error(), "NOT sent") {
		t.Errorf("error must state the message was not delivered, got: %v", err)
	}
	if !strings.Contains(err.Error(), "+15550142") {
		t.Errorf("error should name the recipient, got: %v", err)
	}
	// A bare "code 21610" leaves the operator to go and look it up.
	if !strings.Contains(err.Error(), "STOP") {
		t.Errorf("error should explain the code, got: %v", err)
	}
}

func TestPhoneSendRefusesWhenNotConnected(t *testing.T) {
	p := NewPhoneAdapter(phoneCfg())
	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: "+15550142", Content: "hello",
	}); err == nil {
		t.Fatal("Send must fail before Start: nothing can leave the process")
	}
}

func TestPhoneSendRefusesEmptyContentAndRecipient(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, nil)
	p := connectedPhone(srv.URL, phoneCfg())

	if err := p.Send(context.Background(), OutboundMessage{RecipientID: "+15550142"}); err == nil {
		t.Error("an empty body must be refused — Twilio would reject it anyway")
	}
	if err := p.Send(context.Background(), OutboundMessage{Content: "orphan"}); err == nil {
		t.Error("a message with no recipient must be refused")
	}
	if fake.count() != 0 {
		t.Errorf("nothing should have been posted, got %d requests", fake.count())
	}
}

// A reply longer than the API limit must arrive in full, as several messages,
// rather than being truncated or rejected.
func TestPhoneSendSplitsLongContent(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, nil)
	p := connectedPhone(srv.URL, phoneCfg())

	long := strings.Repeat("a", phoneMaxBodyLen+200)
	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: "+15550142", Content: long,
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if fake.count() < 2 {
		t.Fatalf("long content posted as %d message(s), want it split", fake.count())
	}
	var rebuilt strings.Builder
	for i := 0; i < fake.count(); i++ {
		body := fake.request(i).Get("Body")
		if len(body) > phoneMaxBodyLen {
			t.Errorf("chunk %d is %d chars, over the %d limit", i, len(body), phoneMaxBodyLen)
		}
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != long {
		t.Error("the split lost or reordered content")
	}
}

// Rate limiting is the one failure worth retrying: it heals on its own.
func TestPhoneSendRetriesOnRateLimit(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, func(n int) (int, string) {
		if n == 1 {
			return http.StatusTooManyRequests, `{"code":20429,"message":"Too Many Requests"}`
		}
		return http.StatusCreated, `{"sid":"SMsent","status":"queued"}`
	})
	p := connectedPhone(srv.URL, phoneCfg())

	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: "+15550142", Content: "hello",
	}); err != nil {
		t.Fatalf("Send should have succeeded on the retry, got %v", err)
	}
	if fake.count() != 2 {
		t.Errorf("made %d attempts, want 2 (one throttled, one accepted)", fake.count())
	}
	if !strings.Contains(p.Status().Details, "rate limited") {
		t.Errorf("status should record the throttling, got %q", p.Status().Details)
	}
}

// Auth and validation errors do not heal, so retrying them only delays the
// honest failure and burns the operator's Twilio quota.
func TestPhoneSendDoesNotRetryUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, func(int) (int, string) {
		return http.StatusUnauthorized, `{"code":20003,"message":"Authenticate"}`
	})
	p := connectedPhone(srv.URL, phoneCfg())

	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: "+15550142", Content: "hello",
	}); err == nil {
		t.Fatal("Send must fail on a rejected credential")
	}
	if fake.count() != 1 {
		t.Errorf("made %d attempts, want exactly 1", fake.count())
	}
}

// Twilio only reports the final delivery outcome to the StatusCallback URL, so
// without it a message that the carrier later refuses fails invisibly.
func TestPhoneSendAsksForDeliveryCallbacks(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, nil)
	p := connectedPhone(srv.URL, signedPhoneCfg())

	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: "+15550142", Content: "hello",
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if got := fake.request(0).Get("StatusCallback"); got != testPublicURL {
		t.Errorf("StatusCallback = %q, want %q", got, testPublicURL)
	}
}

// The recipient falls back to ThreadID, which is what the inbound path sets.
func TestPhoneSendFallsBackToThreadID(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, nil)
	p := connectedPhone(srv.URL, phoneCfg())

	if err := p.Send(context.Background(), OutboundMessage{
		ThreadID: "+15550142", Content: "reply",
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if got := fake.request(0).Get("To"); got != "+15550142" {
		t.Errorf("To = %q, want the thread id", got)
	}
}

// A round trip through both halves: an inbound webhook, then a reply to the
// sender it identified.
func TestPhoneRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	fake, srv := newFakeTwilio(t, nil)
	p := connectedPhone(srv.URL, phoneCfg())

	postForm(p, inboundForm("SM-rt", "+15550142", "are you there?"), "")
	got := drainPhone(p)
	if len(got) != 1 {
		t.Fatalf("received %d messages, want 1", len(got))
	}

	if err := p.Send(context.Background(), OutboundMessage{
		RecipientID: got[0].SenderID,
		ThreadID:    got[0].ThreadID,
		Content:     "I am",
	}); err != nil {
		t.Fatalf("reply failed: %v", err)
	}
	if to := fake.request(0).Get("To"); to != "+15550142" {
		t.Errorf("the reply went to %q, not the sender", to)
	}
}

// The handled-set is bounded, and eviction must drop the oldest sids rather
// than growing without limit in a long-lived session.
func TestPhoneHandledSetEvictsOldest(t *testing.T) {
	p := NewPhoneAdapter(msg.ChannelConfig{})
	for i := 0; i < phoneHandledCap+5; i++ {
		if !p.markHandled(fmt.Sprintf("SM%d", i)) {
			t.Fatalf("distinct sid %d reported as duplicate", i)
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.handled) > phoneHandledCap {
		t.Errorf("handled set grew to %d, cap is %d", len(p.handled), phoneHandledCap)
	}
	if len(p.handledOrder) > phoneHandledCap {
		t.Errorf("handled order grew to %d, cap is %d", len(p.handledOrder), phoneHandledCap)
	}
}

// splitMessage is shared by every channel that chunks long replies. Cutting at
// a fixed byte offset used to split the last character of a non-ASCII reply in
// half — a humanoid answering in Turkish would send a broken glyph and start
// the next message with its other half.
func TestSplitMessageNeverBreaksARune(t *testing.T) {
	// "ş" is two bytes, so a 10-byte budget lands mid-character.
	s := strings.Repeat("ş", 20)
	for _, max := range []int{3, 7, 10, 11} {
		chunks := splitMessage(s, max)
		for i, c := range chunks {
			if !utf8.ValidString(c) {
				t.Errorf("max=%d chunk %d is not valid UTF-8: %q", max, i, c)
			}
			if len(c) > max {
				t.Errorf("max=%d chunk %d is %d bytes, over the limit", max, i, len(c))
			}
		}
		if got := strings.Join(chunks, ""); got != s {
			t.Errorf("max=%d: rejoined content differs from the original", max)
		}
	}
}

// The newline-boundary preference must survive the rune-safety change.
func TestSplitMessagePrefersNewlines(t *testing.T) {
	s := strings.Repeat("a", 60) + "\n" + strings.Repeat("b", 60)
	chunks := splitMessage(s, 100)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "\n") {
		t.Errorf("first chunk should end at the newline, got %q", chunks[0][len(chunks[0])-5:])
	}
}
