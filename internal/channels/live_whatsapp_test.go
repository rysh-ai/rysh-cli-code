// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

package channels

// LV1 WhatsApp proof (P-5 "WhatsApp direct (Cloud API endpoint)", design 010).
//
// This targets the DIRECT adapter — whatsapp.go talking to graph.facebook.com
// with its own token and its own webhook — and not relay mode
// (whatsapp_relay.go), where rysh-server owns the token and the webhook and this
// process only dials out over NATS. Direct is the row P-5 names, and it is the
// one a single Meta app can prove on its own; a relay proof is a proof of
// rysh-server's WhatsApp connection as much as of this adapter, and needs a
// configured server workspace and connection id besides.
//
// Two tests, split the same way as Telegram and Teams:
//
//   - TestLiveWhatsAppOutbound needs a token, a phone_number_id and a recipient.
//     It proves the Graph token authenticates and Meta accepts a send. It sends a
//     TEMPLATE, not free-form text: outside the 24h customer-service window Meta
//     only delivers pre-approved templates, and a cold test run is always
//     outside it — Send() refuses free-form text there before it reaches the
//     network. "hello_world" is the template Meta pre-approves on every new app.
//   - TestLiveWhatsAppRoundTrip additionally needs a HUMAN holding the recipient
//     phone and a public tunnel to the webhook, because Meta delivers inbound
//     only to the Callback URL registered on the app. It closes the loop:
//     template out → human reply in through the webhook → a free-form reply
//     back, which is only legal because that inbound opened the 24h window.
//
// Like every live_*_test.go, a green run here is evidence about one run, not a
// claim that WhatsApp is verified.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// whatsAppIsolateState moves the test into a temp working directory. The
// adapter persists its message store under .rysh/channel-state/whatsapp/,
// relative to the CWD (state.go), and the CWD of `go test` is this package's
// source directory.
func whatsAppIsolateState(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

// freeTCPPort returns a port nothing is listening on right now. The direct
// adapter always binds a webhook listener in Start, even when the test never
// uses it, so the outbound test must not collide with a running humanoid on the
// adapter's default 23310.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// liveWhatsAppAdapter builds and starts a direct-mode adapter on port,
// registering Stop as cleanup.
func liveWhatsAppAdapter(t *testing.T, env map[string]string, port int) *WhatsAppAdapter {
	t.Helper()
	adapter := NewWhatsAppAdapter(msg.ChannelConfig{
		Enabled:     true,
		APIKey:      env["RYSH_LIVE_WHATSAPP_ACCESS_TOKEN"],
		Phone:       env["RYSH_LIVE_WHATSAPP_PHONE_NUMBER_ID"],
		AppSecret:   os.Getenv("RYSH_LIVE_WHATSAPP_APP_SECRET"),
		VerifyToken: os.Getenv("RYSH_LIVE_WHATSAPP_VERIFY_TOKEN"),
		WebhookPort: strconv.Itoa(port),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Start validates only that the fields are present and the port binds; the
	// token is first exercised by the send below.
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("whatsapp Start (webhook listen on :%d): %v", port, err)
	}
	t.Cleanup(func() { _ = adapter.Stop() })
	return adapter
}

// whatsAppTemplate returns the template name and language to send.
func whatsAppTemplate() (name, lang string) {
	name = strings.TrimSpace(os.Getenv("RYSH_LIVE_WHATSAPP_TEMPLATE"))
	if name == "" {
		name = "hello_world"
	}
	lang = strings.TrimSpace(os.Getenv("RYSH_LIVE_WHATSAPP_TEMPLATE_LANG"))
	if lang == "" {
		lang = whatsAppTemplateLangDefault
	}
	return name, lang
}

// TestLiveWhatsAppOutbound proves the automatable half: the Graph access token
// authenticates against the phone_number_id, and the Cloud API accepts a
// template send to a real recipient (HTTP 2xx from /{phone_number_id}/messages).
//
// HONEST LIMIT: a 2xx means Meta ACCEPTED the message, not that it was
// delivered. Delivery arrives later as a statuses[] webhook, which needs the
// tunnel TestLiveWhatsAppRoundTrip sets up. The recipient's phone is the visual
// confirmation for this test.
//
// Env:
//
//	RYSH_LIVE_WHATSAPP_ACCESS_TOKEN     Graph API access token with
//	                                    whatsapp_business_messaging (a temporary
//	                                    token from the app's API Setup page works
//	                                    for 24h; a System User token does not expire)
//	RYSH_LIVE_WHATSAPP_PHONE_NUMBER_ID  the numeric phone_number_id from API
//	                                    Setup — NOT the display phone number
//	RYSH_LIVE_WHATSAPP_TO               recipient wa_id, digits only with country
//	                                    code ("447700900123"). On a test number it
//	                                    must be one of the app's (max 5) verified
//	                                    recipient numbers
//	RYSH_LIVE_WHATSAPP_TEMPLATE         template name (optional; default
//	                                    "hello_world", which takes no parameters)
//	RYSH_LIVE_WHATSAPP_TEMPLATE_LANG    its language code (optional; default en_US)
//
// What a human must do first: create a Meta app with the WhatsApp product, keep
// the test phone number Meta assigns, and add the recipient phone under "To"
// in API Setup (Meta texts it a confirmation code).
func TestLiveWhatsAppOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_WHATSAPP_ACCESS_TOKEN",
		"RYSH_LIVE_WHATSAPP_PHONE_NUMBER_ID",
		"RYSH_LIVE_WHATSAPP_TO",
	)
	whatsAppIsolateState(t)
	adapter := liveWhatsAppAdapter(t, env, freeTCPPort(t))
	to := env["RYSH_LIVE_WHATSAPP_TO"]

	// The precondition the template exists for: a fresh adapter has no inbound
	// from this recipient, so free-form text must be refused locally.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := adapter.Send(ctx, OutboundMessage{RecipientID: to, Content: "must not send"}); err == nil ||
		!strings.Contains(err.Error(), "24h") {
		t.Fatalf("free-form Send outside the 24h window = %v, want the local 24h-window refusal", err)
	}

	name, lang := whatsAppTemplate()
	if err := adapter.SendTemplate(ctx, to, name, lang, nil); err != nil {
		t.Fatalf("whatsapp SendTemplate(%s/%s) to %s: %v", name, lang, to, err)
	}
	t.Logf("Cloud API accepted template %q (%s) for %s — confirm on the recipient's phone", name, lang, to)
	if st := adapter.Status(); !st.Connected {
		t.Fatalf("adapter not connected after a successful send: %s", st.Error)
	}
}

// TestLiveWhatsAppRoundTrip is the full direct-mode round trip, and it is
// HUMAN-IN-THE-LOOP by construction — it is gated on RYSH_LIVE_WHATSAPP_TUNNEL_URL,
// not on the credentials alone, so the nightly never waits on a human.
//
// Why it cannot be automated: Meta delivers inbound messages only to the
// Callback URL registered on the app, which must be public HTTPS, and only a
// real WhatsApp user can make Meta send one. A local POST would prove the
// parser, which whatsapp_test.go already does.
//
// The flow:
//  1. send the template — it opens the conversation on the human's phone;
//  2. the human replies with text containing the nonce printed in the log;
//  3. that message must arrive on InboundCh through the webhook, from the
//     recipient, with a wamid (the dedupe key);
//  4. the adapter then sends a FREE-FORM reply. That is legal only inside the
//     24h window, and Send() only believes the window is open because step 3
//     stored the inbound — so a 2xx here proves the store-to-window path too.
//
// Env (in addition to ACCESS_TOKEN / PHONE_NUMBER_ID / TO / TEMPLATE* above):
//
//	RYSH_LIVE_WHATSAPP_TUNNEL_URL     public https URL forwarding to
//	                                  127.0.0.1:<webhook port>, registered as the
//	                                  app's Callback URL with the "messages"
//	                                  webhook field subscribed
//	RYSH_LIVE_WHATSAPP_WEBHOOK_PORT   port the tunnel forwards to (optional;
//	                                  default 23310, the adapter's own)
//	RYSH_LIVE_WHATSAPP_VERIFY_TOKEN   the Verify Token typed into Meta's webhook
//	                                  form (optional; needed only if the Callback
//	                                  URL is (re)verified while this test runs)
//	RYSH_LIVE_WHATSAPP_APP_SECRET     the app secret (optional; when set, every
//	                                  inbound POST must carry a valid
//	                                  X-Hub-Signature-256, as in production)
//	RYSH_LIVE_WHATSAPP_WAIT_SECS      how long to wait for the human (optional;
//	                                  default 180)
//
// A human must: run a tunnel (`cloudflared tunnel --url http://127.0.0.1:23310`);
// set the app's Callback URL to it and subscribe the "messages" field; then,
// while this test waits, reply on the recipient phone to the template message
// with any text containing the nonce from the log.
func TestLiveWhatsAppRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_WHATSAPP_ACCESS_TOKEN",
		"RYSH_LIVE_WHATSAPP_PHONE_NUMBER_ID",
		"RYSH_LIVE_WHATSAPP_TO",
		"RYSH_LIVE_WHATSAPP_TUNNEL_URL",
	)
	whatsAppIsolateState(t)
	port := envInt("RYSH_LIVE_WHATSAPP_WEBHOOK_PORT", defaultWhatsAppWebhookPort)
	adapter := liveWhatsAppAdapter(t, env, port)
	to := env["RYSH_LIVE_WHATSAPP_TO"]

	whatsAppProbeTunnel(t, strings.TrimRight(strings.TrimSpace(env["RYSH_LIVE_WHATSAPP_TUNNEL_URL"]), "/"), port)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	name, lang := whatsAppTemplate()
	if err := adapter.SendTemplate(ctx, to, name, lang, nil); err != nil {
		t.Fatalf("whatsapp SendTemplate(%s/%s) to %s: %v", name, lang, to, err)
	}

	nonce := fmt.Sprintf("rysh-lv1-whatsapp-%d", time.Now().UnixNano())
	wait := time.Duration(envInt("RYSH_LIVE_WHATSAPP_WAIT_SECS", 180)) * time.Second
	t.Logf("HUMAN STEP — template %q sent to %s. Within %s, reply on that phone with text containing %s",
		name, to, wait, nonce)

	got := awaitInbound(t, adapter.InboundCh(), wait, func(m InboundMessage) bool {
		if m.SenderID == to && !strings.Contains(m.Content, nonce) {
			t.Logf("recipient replied with %d chars without the nonce — resend including %q",
				len(m.Content), nonce)
		}
		return strings.Contains(m.Content, nonce)
	})
	if got.SenderID != to {
		t.Fatalf("inbound SenderID = %q, want the recipient %q", got.SenderID, to)
	}
	if got.ThreadID != to {
		t.Fatalf("inbound ThreadID = %q, want the sender wa_id %q (WhatsApp threads per sender)", got.ThreadID, to)
	}
	if got.Metadata["message_id"] == "" {
		t.Fatalf("inbound has no message_id — Meta's webhook retries could not be deduplicated")
	}
	if pn := got.Metadata["phone_number_id"]; pn != env["RYSH_LIVE_WHATSAPP_PHONE_NUMBER_ID"] {
		t.Fatalf("inbound phone_number_id = %q, want %q — the webhook delivered another number's traffic",
			pn, env["RYSH_LIVE_WHATSAPP_PHONE_NUMBER_ID"])
	}
	t.Logf("inbound OK: sender=%s name=%q wamid_set=true content_len=%d signature_checked=%v",
		got.SenderID, got.SenderName, len(got.Content), os.Getenv("RYSH_LIVE_WHATSAPP_APP_SECRET") != "")

	// The window is open now only because the inbound above was stored.
	if err := adapter.Send(ctx, OutboundMessage{
		RecipientID: to,
		Content:     "rysh LV1 reply " + nonce,
	}); err != nil {
		t.Fatalf("free-form reply inside the 24h window: %v", err)
	}
	t.Logf("round-trip OK: free-form reply accepted for %s — confirm it on the phone (nonce %s)", to, nonce)
}

// whatsAppProbeTunnel checks that the tunnel reaches this adapter before a
// human is asked to do anything. A GET without hub.* parameters is answered by
// handleVerify with 403, so a 403 is what a correctly wired tunnel returns.
func whatsAppProbeTunnel(t *testing.T, tunnelURL string, port int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, tunnelURL, nil)
	if err != nil {
		t.Fatalf("RYSH_LIVE_WHATSAPP_TUNNEL_URL %q is not a usable URL: %v", tunnelURL, err)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("tunnel %s is unreachable: %v — start it and point it at 127.0.0.1:%d", tunnelURL, err, port)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Logf("tunnel preflight OK: GET %s → 403 (webhook verification refused), so it terminates at this adapter (:%d)",
			tunnelURL, port)
		return
	}
	t.Logf("tunnel preflight INCONCLUSIVE: GET %s → HTTP %d (403 expected from the adapter). "+
		"If no reply arrives below, check that the tunnel reaches 127.0.0.1:%d", tunnelURL, resp.StatusCode, port)
}
