// SPDX-License-Identifier: Apache-2.0

package channels

// e20 review of gap05-level-e: WhatsApp's api_base was checked only in Start,
// but Validate (rysh setup / doctor, before Start) and every Graph POST
// (Send, SendTemplate) send "Bearer <token>" to it. A cleartext remote
// api_base must be refused wherever a Graph request is built, before any
// request is made.

import (
	"context"
	"net/http"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// gap05NoRequest fails the test if the adapter makes any request at all.
type gap05NoRequest struct{ t *testing.T }

func (g gap05NoRequest) RoundTrip(r *http.Request) (*http.Response, error) {
	g.t.Errorf("a Graph request was made to %s (the token would have been sent)", r.URL.Host)
	return nil, http.ErrHandlerTimeout
}

func TestWhatsAppCleartextAPIBaseNeverGetsTheToken(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, base := range []string{"http://graph.example.com", "http://10.0.0.5:8080", "http://localhost.evil.com"} {
		w := NewWhatsAppAdapter(msg.ChannelConfig{
			Phone: "100000000000001", APIKey: "gap05-synthetic", AppSecret: "gap05-synthetic-secret", APIBase: base,
		})
		w.client = &http.Client{Transport: gap05NoRequest{t}}
		if err := w.Validate(context.Background()); err == nil {
			t.Errorf("Validate accepted api_base %q", base)
		}
		if err := w.postMessages(context.Background(), map[string]any{"messaging_product": "whatsapp"}); err == nil {
			t.Errorf("a Graph POST accepted api_base %q", base)
		}
	}
}
