package channels

// R5: slack.go (729 lines) had no adapter-level tests — only its markdown
// renderer was covered — and factory.go had none at all. These cover the parts
// reachable without a live Socket Mode connection: allow-listing, reply mode,
// dedup expiry, status reporting, and the factory's construction contract.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestSlackIsAllowedChannel(t *testing.T) {
	s := NewSlackAdapter(msg.ChannelConfig{
		BotToken: "xoxb-test", AppToken: "xapp-test",
		Channels: []string{"#C123", "support"},
	})
	// Start() builds allowedChannels (stripping a leading '#'); construct that
	// state directly so the predicate can be tested without a live connection.
	s.allowedChannels = map[string]bool{"C123": true, "support": true}

	// Direct ID match needs no API call.
	if !s.isAllowedChannel("C123") {
		t.Error("an allow-listed channel ID must be permitted")
	}
	// A channel that is neither an allow-listed ID nor resolvable (the client
	// is nil outside a live session) must fail CLOSED.
	if s.isAllowedChannel("C999") {
		t.Error("an unknown channel must be rejected when the name cannot be resolved")
	}
}

// TestSlackEmptyAllowlistIsFailOpen documents the real admission semantics,
// which live at the CALL SITE (handleEventsAPI) rather than in the predicate:
//
//	if len(s.allowedChannels) > 0 && !s.isAllowedChannel(ev.Channel) { drop }
//
// So declaring no channels means "respond wherever the bot is invited" — a
// deliberate fail-OPEN, unlike the humanoid sender allowlist (design 003),
// which fails closed. Being in a Slack channel is itself the admission
// decision there. This test exists so that stays a choice rather than drifting.
func TestSlackEmptyAllowlistIsFailOpen(t *testing.T) {
	s := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb", AppToken: "xapp"})
	if len(s.allowedChannels) != 0 {
		t.Fatalf("expected no allow-list, got %v", s.allowedChannels)
	}
	// The predicate itself rejects (no map entry, no client to resolve with)...
	if s.isAllowedChannel("C123") {
		t.Error("the predicate should not admit an unresolvable channel")
	}
	// ...but the caller skips it entirely when the list is empty, which is what
	// makes the overall behaviour permissive.
}

func TestSlackSetReplyMode(t *testing.T) {
	s := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb", AppToken: "xapp"})
	s.SetReplyMode("mentions")
	s.mu.RLock()
	got := s.replyMode
	s.mu.RUnlock()
	if got != "mentions" {
		t.Errorf("replyMode = %q, want mentions", got)
	}
}

func TestSlackStatusBeforeStart(t *testing.T) {
	s := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb", AppToken: "xapp"})
	st := s.Status()
	if st.Type != "slack" {
		t.Errorf("Type = %q, want slack", st.Type)
	}
	if st.Connected {
		t.Error("an adapter that never started must not report Connected")
	}
}

// TestSlackMentionDedupExpiry: the dedup map is swept by age, so a long-running
// session cannot leak an entry per mention forever.
func TestSlackMentionDedupExpiry(t *testing.T) {
	s := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb", AppToken: "xapp"})
	s.mentionDedup["fresh"] = time.Now()
	s.mentionDedup["stale"] = time.Now().Add(-time.Minute) // older than the 30s window

	s.cleanMentionDedup()

	if _, ok := s.mentionDedup["stale"]; ok {
		t.Error("a stale dedup entry must be swept")
	}
	if _, ok := s.mentionDedup["fresh"]; !ok {
		t.Error("a fresh dedup entry must survive — sweeping it would re-deliver the mention")
	}
}

// TestSlackStartRejectsNonAppToken pins the offline shape check in Start.
// The regression it guards: an app-level token is the only credential Socket
// Mode needs and the only one auth.test does not exercise, so a wrong value
// used to pass startup, get reported as "[connected]", and fail asynchronously
// with invalid_auth where nothing but a log line could see it. The concrete
// case was the Signing Secret — 64 hex characters, sitting one box above
// App-Level Tokens on the same Slack settings page.
func TestSlackStartRejectsNonAppToken(t *testing.T) {
	const signingSecret = "7b180de7470736dd4605eefcd83e952143b764479d2ea41d4d66e35f2d85fb42"
	s := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-test", AppToken: signingSecret})

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("Start accepted a non-xapp app_token")
	}
	if !strings.Contains(err.Error(), "xapp-") {
		t.Errorf("error must name the expected form, got %v", err)
	}
	// The check must run BEFORE the network, so a bad token is reported
	// instantly rather than behind an auth.test round trip.
	if strings.Contains(err.Error(), "auth.test") {
		t.Errorf("shape check must precede auth.test, got %v", err)
	}
	// A credential must never be echoed into an error that reaches the pane.
	if strings.Contains(err.Error(), signingSecret[:8]) {
		t.Errorf("error leaked part of the credential: %v", err)
	}
	if s.Status().Connected {
		t.Error("a rejected Start must not leave the adapter reporting Connected")
	}
}

// TestSlackStartAndValidateAgreeOnAppToken is a drift guard: doctor (Validate)
// and channel start (Start) must reject the same app tokens. They diverged
// once — Validate checked the xapp- prefix and Start did not — which is why
// `rysh doctor` could report a problem the wizard had just called connected.
func TestSlackStartAndValidateAgreeOnAppToken(t *testing.T) {
	for _, tok := range []string{"", "not-a-token", strings.Repeat("a", 64)} {
		s := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-test", AppToken: tok})
		startErr := s.Start(context.Background())
		validateErr := s.Validate(context.Background())
		if (startErr == nil) != (validateErr == nil) {
			t.Errorf("app_token %q: Start err=%v but Validate err=%v — the two gates have drifted",
				tok, startErr, validateErr)
		}
	}
}

func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("short", 10); got != "short" {
		t.Errorf("under the limit should pass through, got %q", got)
	}
	if got := truncateForLog("0123456789abc", 10); got != "0123456789..." {
		t.Errorf("truncateForLog = %q", got)
	}
}

// --- factory.go ---

func TestFactoryConstructsEveryValidType(t *testing.T) {
	// Every type in ValidChannelTypes must be constructible, and must report
	// the type it was asked for — a mismatch here would route a humanoid's
	// messages to the wrong adapter.
	for _, ct := range ValidChannelTypes {
		cfg := msg.ChannelConfig{Enabled: true}
		a, err := NewAdapter(ct, cfg)
		if err != nil {
			t.Errorf("NewAdapter(%q) failed: %v", ct, err)
			continue
		}
		if a == nil {
			t.Errorf("NewAdapter(%q) returned nil without an error", ct)
			continue
		}
		if got := a.Type(); got != ct {
			t.Errorf("NewAdapter(%q).Type() = %q", ct, got)
		}
	}
}

func TestFactoryRejectsUnknownType(t *testing.T) {
	if _, err := NewAdapter("carrier-pigeon", msg.ChannelConfig{}); err == nil {
		t.Error("an unknown channel type must be an error, not a nil adapter")
	} else if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("error should name the offending type, got %v", err)
	}
}

func TestIsValidChannelType(t *testing.T) {
	for _, ct := range ValidChannelTypes {
		if !IsValidChannelType(ct) {
			t.Errorf("IsValidChannelType(%q) = false", ct)
		}
	}
	if IsValidChannelType("carrier-pigeon") {
		t.Error("IsValidChannelType accepted an unknown type")
	}
}
