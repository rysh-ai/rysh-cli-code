package channels

// Inbound half of the Twilio SMS adapter: webhook parsing, signature
// authentication, deduplication, status callbacks and the SMS-only boundary.
// The outbound half lives in phone_send_test.go.

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const testPublicURL = "https://tunnel.example/sms"

func phoneCfg() msg.ChannelConfig {
	return msg.ChannelConfig{
		AccountSID: "ACtest",
		AuthToken:  "tok-test",
		Number:     "+15550100",
		Provider:   "twilio",
	}
}

// signedPhoneCfg enables inbound signature validation by declaring the public
// URL Twilio posts to.
func signedPhoneCfg() msg.ChannelConfig {
	cfg := phoneCfg()
	cfg.WebhookURL = testPublicURL
	return cfg
}

// inboundForm is a minimal Twilio "a message came in" webhook body.
func inboundForm(sid, from, body string) url.Values {
	return url.Values{
		"MessageSid": {sid},
		"SmsStatus":  {"received"},
		"From":       {from},
		"To":         {"+15550100"},
		"Body":       {body},
		"NumMedia":   {"0"},
	}
}

// postForm drives the webhook handler directly, without binding a port.
func postForm(p *PhoneAdapter, form url.Values, signature string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if signature != "" {
		req.Header.Set("X-Twilio-Signature", signature)
	}
	p.handleWebhook(rec, req)
	return rec
}

// signTwilio produces the signature Twilio would send for this body.
func signTwilio(publicURL string, form url.Values, authToken string) string {
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(publicURL))
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range form[k] {
			mac.Write([]byte(k))
			mac.Write([]byte(v))
		}
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// drainPhone reports how many inbound messages reached the humanoid.
func drainPhone(p *PhoneAdapter) []InboundMessage {
	var out []InboundMessage
	for {
		select {
		case im := <-p.inbound:
			out = append(out, im)
		default:
			return out
		}
	}
}

func TestPhoneStartRequiresCredentials(t *testing.T) {
	// Each missing key must be named in the error: "phone: not configured"
	// sends the operator hunting through a skill file.
	cases := []struct {
		name string
		cfg  msg.ChannelConfig
		want string
	}{
		{"no account_sid", msg.ChannelConfig{AuthToken: "t", Number: "+1"}, "account_sid"},
		{"no auth_token", msg.ChannelConfig{AccountSID: "AC", Number: "+1"}, "auth_token"},
		{"no number", msg.ChannelConfig{AccountSID: "AC", AuthToken: "t"}, "number"},
		{"other provider", msg.ChannelConfig{AccountSID: "AC", AuthToken: "t", Number: "+1", Provider: "vonage"}, "twilio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPhoneAdapter(tc.cfg)
			err := p.Start(context.Background())
			if err == nil {
				t.Fatal("Start must fail when a credential is missing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %v", tc.want, err)
			}
			if p.Status().Connected {
				t.Error("Status must not report Connected after a failed Start")
			}
		})
	}
}

func TestPhoneInboundWebhookReachesTheHumanoid(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	rec := postForm(p, inboundForm("SM1", "+15550142", "what is rysh?"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", rec.Code)
	}
	// The response must be TwiML Twilio can parse, and must not contain a
	// reply — the humanoid answers asynchronously over the REST API.
	if !strings.Contains(rec.Body.String(), "<Response></Response>") {
		t.Errorf("webhook should answer with empty TwiML, got %q", rec.Body.String())
	}

	got := drainPhone(p)
	if len(got) != 1 {
		t.Fatalf("humanoid received %d messages, want 1", len(got))
	}
	im := got[0]
	if im.SenderID != "+15550142" {
		t.Errorf("SenderID = %q", im.SenderID)
	}
	if im.Content != "what is rysh?" {
		t.Errorf("Content = %q", im.Content)
	}
	// Keying the thread per sender is what gives the humanoid one conversation
	// per correspondent — SMS has no thread of its own.
	if im.ThreadID != "+15550142" {
		t.Errorf("ThreadID = %q, want the sender number", im.ThreadID)
	}
	if im.Metadata["message_sid"] != "SM1" {
		t.Errorf("message_sid metadata = %q", im.Metadata["message_sid"])
	}
	// The humanoid reads metadata["channel"] as an outbound recipient, so the
	// key must never appear (the bug whatsapp.go's comment records).
	if _, bad := im.Metadata["channel"]; bad {
		t.Error(`metadata must not carry a "channel" key`)
	}
}

// Twilio retries a webhook whose response it did not like, and the retry
// carries the same MessageSid. Handling it again would answer the sender twice
// — the same customer-visible failure the WhatsApp relay hit.
func TestPhoneInboundDedupesRetriedWebhook(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	form := inboundForm("SM-dup", "+15550142", "hello")
	postForm(p, form, "")
	postForm(p, form, "")

	if got := drainPhone(p); len(got) != 1 {
		t.Fatalf("humanoid received %d copies of one message, want 1", len(got))
	}
	if got := len(p.RecentMessages(0)); got != 1 {
		t.Errorf("message store holds %d copies, want 1", got)
	}
}

// A restart is the case that matters: the process comes back, Twilio retries a
// webhook it never got a 200 for, and the new process must recognise it.
func TestPhoneDedupeSurvivesRestart(t *testing.T) {
	t.Chdir(t.TempDir())

	first := NewPhoneAdapter(phoneCfg())
	form := inboundForm("SM-answered", "+15550142", "already handled")
	postForm(first, form, "")
	if got := drainPhone(first); len(got) != 1 {
		t.Fatalf("setup: first run received %d, want 1", len(got))
	}

	second := NewPhoneAdapter(phoneCfg())
	postForm(second, form, "")
	if got := drainPhone(second); len(got) != 0 {
		t.Fatalf("after restart the retried webhook was answered again (%d dispatches)", len(got))
	}
}

func TestPhoneRejectsForgedSignature(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(signedPhoneCfg())

	form := inboundForm("SM-forged", "+15550142", "transfer the funds")
	rec := postForm(p, form, base64.StdEncoding.EncodeToString([]byte("not the real mac")))

	if rec.Code != http.StatusForbidden {
		t.Errorf("forged signature status = %d, want 403", rec.Code)
	}
	if got := drainPhone(p); len(got) != 0 {
		t.Fatalf("a forged webhook reached the humanoid (%d messages)", len(got))
	}
}

func TestPhoneAcceptsValidSignature(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := signedPhoneCfg()
	p := NewPhoneAdapter(cfg)

	form := inboundForm("SM-signed", "+15550142", "genuine")
	rec := postForm(p, form, signTwilio(testPublicURL, form, cfg.AuthToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("signed webhook status = %d, want 200", rec.Code)
	}
	if got := drainPhone(p); len(got) != 1 {
		t.Fatalf("signed webhook delivered %d messages, want 1", len(got))
	}
}

// Without webhook_url the signature cannot be checked at all (Twilio signs the
// public URL). That is a real weakening, so it must be visible in Status()
// rather than looking identical to a verified channel.
func TestPhoneStatusDisclosesUnverifiedInbound(t *testing.T) {
	unverified := NewPhoneAdapter(phoneCfg()).Status()
	if !strings.Contains(unverified.Details, "UNVERIFIED") {
		t.Errorf("status should disclose that inbound is unauthenticated, got %q", unverified.Details)
	}
	verified := NewPhoneAdapter(signedPhoneCfg()).Status()
	if strings.Contains(verified.Details, "UNVERIFIED") {
		t.Errorf("a configured webhook_url must clear the warning, got %q", verified.Details)
	}
}

// Delivery receipts arrive on the same endpoint as inbound messages. Emitting
// one as an InboundMessage would have the humanoid compose a reply to a status
// callback and text it to the customer.
func TestPhoneStatusCallbackIsNotAConversation(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	postForm(p, url.Values{
		"MessageSid":    {"SMout1"},
		"MessageStatus": {"delivered"},
		"To":            {"+15550142"},
		"From":          {"+15550100"},
	}, "")

	if got := drainPhone(p); len(got) != 0 {
		t.Fatalf("a delivery receipt was forwarded as a message (%d)", len(got))
	}
}

func TestPhoneFailedDeliverySurfacesInStatus(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	postForm(p, url.Values{
		"MessageSid":    {"SMout2"},
		"MessageStatus": {"undelivered"},
		"To":            {"+15550142"},
		"ErrorCode":     {"21610"},
	}, "")

	st := p.Status()
	if !strings.Contains(st.Details, "21610") {
		t.Errorf("status should carry the failure code, got %q", st.Details)
	}
	// The code alone is unreadable; the operator needs to know the recipient
	// opted out rather than that "something failed".
	if !strings.Contains(st.Details, "STOP") {
		t.Errorf("status should explain error 21610, got %q", st.Details)
	}
}

// This adapter is SMS-only. A voice webhook must be refused audibly rather than
// answered with silence, and must never become a message.
func TestPhoneVoiceCallIsRejectedNotAnswered(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	rec := postForm(p, url.Values{
		"CallSid":    {"CA123"},
		"From":       {"+15550142"},
		"To":         {"+15550100"},
		"CallStatus": {"ringing"},
	}, "")

	if !strings.Contains(rec.Body.String(), "<Reject/>") {
		t.Errorf("a voice call should be rejected explicitly, got %q", rec.Body.String())
	}
	if got := drainPhone(p); len(got) != 0 {
		t.Fatalf("a voice call became a message (%d)", len(got))
	}
}

// MMS media are not downloaded. Handing the humanoid a message whose picture
// silently vanished would have it answer as though it had seen one.
func TestPhoneMMSDisclosesUndownloadedMedia(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	form := inboundForm("SM-mms", "+15550142", "look at this")
	form.Set("NumMedia", "2")
	postForm(p, form, "")

	got := drainPhone(p)
	if len(got) != 1 {
		t.Fatalf("received %d messages, want 1", len(got))
	}
	if !strings.Contains(got[0].Content, "not downloaded") {
		t.Errorf("content should disclose the dropped media, got %q", got[0].Content)
	}
	if got[0].Metadata["num_media"] != "2" {
		t.Errorf("num_media metadata = %q", got[0].Metadata["num_media"])
	}
}

// A webhook with neither body nor media carries nothing to answer.
func TestPhoneIgnoresEmptyInbound(t *testing.T) {
	t.Chdir(t.TempDir())
	p := NewPhoneAdapter(phoneCfg())

	postForm(p, url.Values{"MessageSid": {"SM-empty"}, "From": {"+15550142"}, "Body": {""}}, "")
	if got := drainPhone(p); len(got) != 0 {
		t.Fatalf("an empty message was forwarded (%d)", len(got))
	}
}

func TestPhoneValidateChecksCredentialsWithoutBinding(t *testing.T) {
	t.Chdir(t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/Accounts/ACtest.json") {
			t.Errorf("unexpected validate path %q", r.URL.Path)
		}
		sid, tok, ok := r.BasicAuth()
		if !ok || sid != "ACtest" || tok != "tok-test" {
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"code":20003,"message":"Authenticate"}`))
			return
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"sid":"ACtest","status":"active"}`))
	}))
	defer srv.Close()

	ok := NewPhoneAdapter(phoneCfg())
	ok.baseURL = srv.URL
	if err := ok.Validate(context.Background()); err != nil {
		t.Errorf("valid credentials should pass, got %v", err)
	}
	// Validate must not have started anything — that is the whole point of the
	// non-binding check `rysh doctor` prefers.
	if ok.Status().Connected {
		t.Error("Validate must not connect the adapter")
	}

	bad := phoneCfg()
	bad.AuthToken = "wrong"
	badAdapter := NewPhoneAdapter(bad)
	badAdapter.baseURL = srv.URL
	err := badAdapter.Validate(context.Background())
	if err == nil {
		t.Fatal("a rejected token must fail validation")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should say the credentials were rejected, got %v", err)
	}
}
