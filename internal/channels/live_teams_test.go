//go:build livechannels

package channels

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The Teams proof (P-5) is deliberately TWO tests, because the two halves are
// gated on different things and design 010 assumed one round trip that no
// unattended runner can perform:
//
//   - TestLiveTeamsOutbound needs only the Azure bot registration (app id +
//     secret, optional tenant). It proves Entra client-credentials auth and that
//     the Connector API accepts what the adapter posts.
//   - TestLiveTeamsInboundRoundTrip additionally needs a PUBLIC TUNNEL and a
//     human, because teams.go binds its Messaging Endpoint to 127.0.0.1 and
//     teams_auth.go verifies the Bot Framework JWT on every activity with no
//     opt-out — so inbound cannot be synthesised locally, only delivered by
//     Microsoft.
//
// Folding them into one test would mean a test that can never run green until
// every one of those exists; split, the auth half runs the day the Azure app
// registration lands.

// teamsLiveConfig builds a ChannelConfig from the shared Teams env vars.
// webhookPort is passed explicitly because the two tests want different
// defaults: the outbound test binds a listener it never uses, the inbound test
// must bind exactly the port the tunnel forwards to.
func teamsLiveConfig(env map[string]string, webhookPort int) msg.ChannelConfig {
	return msg.ChannelConfig{
		Enabled:   true,
		AppID:     strings.TrimSpace(env["RYSH_LIVE_TEAMS_APP_ID"]),
		AppSecret: env["RYSH_LIVE_TEAMS_APP_SECRET"],
		// Optional: requireEnv only fills the keys it was asked to require, so an
		// optional value must be read from the environment directly.
		TenantID:    strings.TrimSpace(os.Getenv("RYSH_LIVE_TEAMS_TENANT_ID")),
		WebhookPort: strconv.Itoa(webhookPort),
	}
}

// teamsIsolateState moves the test into a temp dir. teams.go persists its
// conversation→serviceUrl routing table and handled-activity set to the RELATIVE
// path .rysh/channel-state/teams/<app id>.json, both on construction
// (loadPersisted) and on every remembered conversation. Without this the test
// would inherit a real session's remembered serviceUrls — which would let the
// outbound test pass without the operator having supplied one — and would litter
// the package directory.
func teamsIsolateState(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

// TestLiveTeamsOutbound is the Teams auth + outbound proof. It acquires a real
// Entra client-credentials token for the Connector scope (Validate — the Teams
// analogue of Slack's auth.test), starts the adapter, teaches it the serviceUrl
// for a known conversation, and posts both outbound renderings.
//
// HONEST LIMIT: the Bot Framework Connector API gives a bot no way to read a
// conversation back. There is no Teams equivalent of conversations.replies, so
// unlike the Slack proof this test can assert only that Microsoft accepted the
// activity with a 2xx. The nonce is logged so a human can confirm both messages
// visually in the Teams client — that human confirmation is the last step of the
// P-5 outbound proof and cannot be automated from here.
//
// Env:
//
//	RYSH_LIVE_TEAMS_APP_ID           the Azure Bot's Microsoft App (client) ID
//	RYSH_LIVE_TEAMS_APP_SECRET       that app registration's client secret
//	RYSH_LIVE_TEAMS_CONVERSATION_ID  e.g. "19:abc…@thread.tacv2" (or a 1:1
//	                                 "a:1a2b…") — the conversation to post into
//	RYSH_LIVE_TEAMS_SERVICE_URL      e.g. https://smba.trafficmanager.net/emea/
//	                                 the regional Connector endpoint that
//	                                 delivered that conversation's activities
//	RYSH_LIVE_TEAMS_TENANT_ID        Entra tenant GUID (optional; empty uses the
//	                                 multi-tenant botframework.com authority —
//	                                 set it only for a single-tenant bot)
//	RYSH_LIVE_TEAMS_WEBHOOK_PORT     loopback port to bind (optional; default
//	                                 23413 — incidental here, chosen away from
//	                                 the adapter's real default 23313 so this
//	                                 test cannot collide with a running session)
//
// A human must, once: register an Azure Bot (Microsoft App ID + client secret);
// create a Teams app manifest carrying that app id and install/sideload it into
// a team or a 1:1 chat; then send the bot ONE message and capture the
// `conversation.id` and `serviceUrl` from that inbound activity (the tunnel run
// below prints both, or Bot Framework Emulator / the Azure "Test in Web Chat"
// blade shows them). Those two values are what this test cannot derive: Teams
// accepts a reply only on the endpoint that delivered a message.
func TestLiveTeamsOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_TEAMS_APP_ID",
		"RYSH_LIVE_TEAMS_APP_SECRET",
		"RYSH_LIVE_TEAMS_CONVERSATION_ID",
		"RYSH_LIVE_TEAMS_SERVICE_URL",
	)
	teamsIsolateState(t)

	cfg := teamsLiveConfig(env, envInt("RYSH_LIVE_TEAMS_WEBHOOK_PORT", 23413))
	adapter := NewTeamsAdapter(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Auth proof, before anything binds: Validate acquires the Connector token
	// and has no side effects, so a bad secret or tenant fails here and nowhere
	// else.
	if err := adapter.Validate(ctx); err != nil {
		t.Fatalf("teams Validate (Entra client-credentials token): %v", err)
	}
	t.Logf("Entra token acquired: app_id=%s tenant=%q multi_tenant=%v",
		cfg.AppID, cfg.TenantID, cfg.TenantID == "")

	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("teams Start (token + messaging endpoint listen on 127.0.0.1:%s): %v",
			cfg.WebhookPort, err)
	}
	defer func() { _ = adapter.Stop() }()
	if st := adapter.Status(); !st.Connected {
		t.Fatalf("teams Status after Start: connected=false, error=%q", st.Error)
	}

	convID := strings.TrimSpace(env["RYSH_LIVE_TEAMS_CONVERSATION_ID"])
	serviceURL := strings.TrimSpace(env["RYSH_LIVE_TEAMS_SERVICE_URL"])
	// This test has no inbound leg, so it does by hand what handleActivity would
	// have done on the first activity: record which regional endpoint the
	// conversation is reachable on. Without it Send refuses, by design.
	adapter.rememberConversation(convID, serviceURL)
	if u, ok := adapter.serviceURLFor(convID); !ok || u != serviceURL {
		t.Fatalf("serviceURL for %s not recorded (got %q, ok=%v)", convID, u, ok)
	}

	nonce := fmt.Sprintf("rysh-lv1-teams-%d", time.Now().UnixNano())
	if err := adapter.Send(ctx, OutboundMessage{
		ThreadID: convID,
		Content:  "LV1 outbound probe " + nonce,
	}); err != nil {
		t.Fatalf("teams Send (message): %v", err)
	}

	// The step rendering goes out as textFormat "xml" with the content escaped
	// (teams.go Send, OutboundKindStep). It is a distinct activity shape the
	// Connector validates separately, so a plain-message pass does not cover it.
	if err := adapter.Send(ctx, OutboundMessage{
		ThreadID: convID,
		Content:  "🔧 LV1 step " + nonce,
		Kind:     OutboundKindStep,
	}); err != nil {
		t.Fatalf("teams Send (step / xml textFormat): %v", err)
	}

	t.Logf("Connector API accepted both activities for conversation %s — "+
		"confirm visually in Teams: search the conversation for %q "+
		"(one plain line and one italic step line)", convID, nonce)
}

// TestLiveTeamsInboundRoundTrip is the full P-5 round trip, and it is
// HUMAN-IN-THE-LOOP by construction — it can never run unattended in the
// nightly, which is why it is gated on RYSH_LIVE_TEAMS_TUNNEL_URL and not on the
// credentials alone.
//
// Why it cannot be automated: teams.go binds the Messaging Endpoint to
// 127.0.0.1, so Microsoft can only reach it through a public tunnel whose URL is
// registered as the Azure Bot's Messaging Endpoint. And teams_auth.go verifies
// the Bot Framework JWT (RS256 against Microsoft's JWKS, issuer, audience =
// this bot's app id, validity window, serviceurl claim) on every activity with
// no config switch — deliberately, since the endpoint is publicly reachable. So
// the inbound leg cannot be synthesised with a local POST: only Microsoft can
// mint an activity this adapter will accept, and only a Teams client (or a Graph
// app with ChannelMessage.Send, a heavier registration than the bot itself) can
// make Microsoft mint one.
//
// The test therefore starts the adapter, checks the tunnel reaches this process,
// prints what to type, waits for the probe, and then replies on the serviceUrl
// the activity taught it — which is the part that proves the routing table, and
// the part TestLiveTeamsOutbound has to be handed by the operator.
//
// Env (in addition to APP_ID / APP_SECRET / optional TENANT_ID above):
//
//	RYSH_LIVE_TEAMS_TUNNEL_URL           the public https URL registered as the
//	                                     Azure Bot's Messaging Endpoint,
//	                                     forwarding to 127.0.0.1:<webhook port>
//	                                     (Azure's convention is a /api/messages
//	                                     path; the adapter mounts at "/" so any
//	                                     path works)
//	RYSH_LIVE_TEAMS_WEBHOOK_PORT         port the tunnel forwards to (optional;
//	                                     default 23313, the adapter's own)
//	RYSH_LIVE_TEAMS_INBOUND_PROBE        text the human sends (optional; default
//	                                     "rysh-lv1-teams"). Fixed rather than a
//	                                     nonce so it can be agreed before the run
//	                                     — matching is case-insensitive substring
//	RYSH_LIVE_TEAMS_INBOUND_TIMEOUT_SEC  how long to wait for it (optional;
//	                                     default 180)
//
// A human must: run a tunnel (`cloudflared tunnel --url http://127.0.0.1:23313`
// or ngrok); paste its URL + /api/messages into the Azure Bot's Messaging
// Endpoint; have the Teams app installed; and, while this test waits, send the
// probe text to the bot — in a 1:1 chat (every message there counts as
// addressed to the bot) or as an @mention in the channel.
func TestLiveTeamsInboundRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_TEAMS_APP_ID",
		"RYSH_LIVE_TEAMS_APP_SECRET",
		"RYSH_LIVE_TEAMS_TUNNEL_URL",
	)
	teamsIsolateState(t)

	port := envInt("RYSH_LIVE_TEAMS_WEBHOOK_PORT", defaultTeamsWebhookPort)
	adapter := NewTeamsAdapter(teamsLiveConfig(env, port))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("teams Start (token + messaging endpoint listen on 127.0.0.1:%d): %v", port, err)
	}
	defer func() { _ = adapter.Stop() }()

	tunnelURL := strings.TrimRight(strings.TrimSpace(env["RYSH_LIVE_TEAMS_TUNNEL_URL"]), "/")
	teamsProbeTunnel(t, tunnelURL, port)

	probe := strings.TrimSpace(os.Getenv("RYSH_LIVE_TEAMS_INBOUND_PROBE"))
	if probe == "" {
		probe = "rysh-lv1-teams"
	}
	wait := time.Duration(envInt("RYSH_LIVE_TEAMS_INBOUND_TIMEOUT_SEC", 180)) * time.Second

	t.Logf("HUMAN STEP — within the next %s, send a Teams message containing %q "+
		"to the bot (1:1 chat, or @mention it in the channel it is installed in)", wait, probe)

	got := awaitInbound(t, adapter.InboundCh(), wait, func(m InboundMessage) bool {
		return strings.Contains(strings.ToLower(m.Content), strings.ToLower(probe))
	})

	// The activity survived JWT verification, the allow-list and mention policy,
	// and was mapped to an InboundMessage keyed on the conversation.
	convID := got.ThreadID
	if convID == "" || convID != got.Metadata["conversation_id"] {
		t.Fatalf("inbound ThreadID = %q, want the conversation id %q",
			convID, got.Metadata["conversation_id"])
	}
	if got.SenderID == "" {
		t.Fatalf("inbound has no SenderID — the activity's from.id was empty")
	}
	if got.Metadata["activity_id"] == "" {
		t.Fatalf("inbound has no activity_id — redelivery could not be deduplicated")
	}
	t.Logf("inbound OK: conversation=%s type=%s sender=%s mention=%s observe_only=%s content_len=%d",
		convID, got.Metadata["conversation_type"], got.SenderName,
		got.Metadata["mention"], got.Metadata["observe_only"], len(got.Content))

	// The serviceUrl must have been learned from that activity — this is the
	// routing fact TestLiveTeamsOutbound has to be handed by the operator.
	serviceURL, ok := adapter.serviceURLFor(convID)
	if !ok {
		t.Fatalf("no serviceUrl remembered for %s after an inbound activity — outbound is impossible", convID)
	}
	t.Logf("serviceUrl learned from the activity: %s", serviceURL)

	nonce := fmt.Sprintf("rysh-lv1-teams-rt-%d", time.Now().UnixNano())
	if err := adapter.Send(ctx, OutboundMessage{
		ThreadID: convID,
		Content:  "LV1 pong " + nonce,
	}); err != nil {
		t.Fatalf("teams Send (reply on the learned serviceUrl): %v", err)
	}
	t.Logf("round-trip OK: reply accepted for conversation %s — confirm %q appeared in Teams", convID, nonce)
}

// teamsProbeTunnel checks that the public URL actually terminates at this
// process's messaging endpoint before the test spends minutes waiting for an
// activity. handleActivity answers anything that is not a POST with 405, so a
// 405 is a positive identification.
//
// A non-405 is logged, not failed: tunnel front-ends (ngrok's free interstitial,
// for one) answer browser-shaped GETs themselves while passing Azure's POSTs
// through, so failing here would reject a working setup. Only an unreachable URL
// is fatal.
func teamsProbeTunnel(t *testing.T, tunnelURL string, port int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, tunnelURL, nil)
	if err != nil {
		t.Fatalf("RYSH_LIVE_TEAMS_TUNNEL_URL %q is not a usable URL: %v", tunnelURL, err)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("tunnel %s is unreachable: %v — start the tunnel and point it at 127.0.0.1:%d, "+
			"then set the Azure Bot's Messaging Endpoint to it", tunnelURL, err, port)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Logf("tunnel preflight OK: GET %s → 405, so it terminates at this adapter's endpoint (127.0.0.1:%d)",
			tunnelURL, port)
		return
	}
	t.Logf("tunnel preflight INCONCLUSIVE: GET %s → HTTP %d (405 expected from the adapter). "+
		"Either the tunnel front-end answered this GET itself, or it does not reach 127.0.0.1:%d — "+
		"if no activity arrives below, check that first", tunnelURL, resp.StatusCode, port)
}
