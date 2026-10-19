// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A fake rysh-server that serves the owner routes with a bearer check, the
// way design 037's key-aware group middleware does.
func fakeCompanyServer(t *testing.T, key string, broken map[string]int) *httptest.Server {
	t.Helper()
	const id = "22222222-2222-2222-2222-222222222222"
	mux := http.NewServeMux()
	route := func(path string, body any) {
		mux.HandleFunc("/api/ai-native-companies/"+id+path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
				return
			}
			if code, ok := broken[path]; ok {
				w.WriteHeader(code)
				return
			}
			_ = json.NewEncoder(w).Encode(body)
		})
	}
	route("", map[string]any{
		"company": map[string]any{"id": id, "workspace_id": "ws-1", "name": "Modern Bistro", "slug": "firat", "fqdn": "firat.dns.rysh.ai", "status": "active", "template_id": "restaurant-modern-bistro"},
		"events":  []map[string]any{{"stage": "deploying_site", "status": "succeeded", "message": "site live", "occurred_at": "2026-09-15T10:00:00Z"}},
	})
	route("/orders", map[string]any{"orders": []map[string]any{{"id": "o1", "reference": "ABC23", "customer_name": "Ada", "fulfilment": "delivery", "amount_pence": 1850, "currency": "GBP", "status": "received", "payment_status": "paid", "created_at": "2026-09-15T11:00:00Z", "items": []map[string]any{{"sku": "pizza", "name": "Pizza", "quantity": 2, "unit_pence": 925, "total_pence": 1850}}}}})
	route("/enquiries", map[string]any{"enquiries": []map[string]any{{"id": "e1", "reference": "QRS45", "kind": "reservation", "name": "Bo", "status": "new", "fields": `{"party_size":"4"}`, "created_at": "2026-09-15T12:00:00Z"}}})
	route("/email", map[string]any{"email": map[string]any{"address": "info@firat.dns.rysh.ai", "incoming_email_address": "info@firat.dns.rysh.ai", "outgoing_email_address": "info@letchat.ai", "state": "active"}})
	route("/phone", map[string]any{"phone": map[string]any{"e164": "+442012345678", "state": "active"}})
	route("/services", map[string]any{"services": []map[string]any{{"service_key": "chatbot", "state": "active", "status_message": "ready", "currency": "GBP"}}})
	route("/channels", map[string]any{"channels": []map[string]any{{"channel": "whatsapp", "enabled": true, "state": "awaiting_credentials", "status_message": "needs a token", "credentials_required": []string{"token"}}}})
	return httptest.NewServer(mux)
}

func TestClientFetchDecodesEveryResource(t *testing.T) {
	srv := fakeCompanyServer(t, "rysh_ok", nil)
	defer srv.Close()
	snap, err := NewClient(srv.URL+"/", "rysh_ok").Fetch(context.Background(), "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.Company.Name != "Modern Bistro" || snap.Company.SiteURL != "https://firat.dns.rysh.ai" || snap.Company.ServerURL != srv.URL {
		t.Fatalf("company: %+v", snap.Company)
	}
	if len(snap.Orders) != 1 || snap.Orders[0].Reference != "ABC23" || snap.Orders[0].AmountPence != 1850 || len(snap.Orders[0].Items) != 1 || snap.Orders[0].CreatedAt.IsZero() {
		t.Fatalf("orders: %+v", snap.Orders)
	}
	if len(snap.Enquiries) != 1 || snap.Enquiries[0].Kind != "reservation" || snap.Enquiries[0].Fields != `{"party_size":"4"}` {
		t.Fatalf("enquiries: %+v", snap.Enquiries)
	}
	if snap.Email == nil || snap.Email.IncomingAddress != "info@firat.dns.rysh.ai" || snap.Email.State != "active" {
		t.Fatalf("email: %+v", snap.Email)
	}
	if snap.Phone == nil || snap.Phone.Number != "+442012345678" || snap.Phone.State != "active" {
		t.Fatalf("phone: %+v", snap.Phone)
	}
	if len(snap.Services) != 1 || snap.Services[0].ServiceKey != "chatbot" || len(snap.Channels) != 1 || snap.Channels[0].Channel != "whatsapp" {
		t.Fatalf("services/channels: %+v / %+v", snap.Services, snap.Channels)
	}
	if len(snap.Activity) != 1 || snap.Activity[0].Kind != "progress" || snap.Activity[0].Summary != "site live" {
		t.Fatalf("activity from provisioning events: %+v", snap.Activity)
	}
	if !snap.Link.Connected || snap.Link.LastError != "" || snap.Link.LastFetch.IsZero() {
		t.Fatalf("link: %+v", snap.Link)
	}
}

func TestClientFetchRefusedKeyIsUnauthorized(t *testing.T) {
	srv := fakeCompanyServer(t, "rysh_ok", nil)
	defer srv.Close()
	_, err := NewClient(srv.URL, "rysh_revoked").Fetch(context.Background(), "22222222-2222-2222-2222-222222222222")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	// A company that is gone reads as the same thing: the key is no good here.
	_, err = NewClient(srv.URL, "rysh_ok").Fetch(context.Background(), "33333333-3333-3333-3333-333333333333")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown company: want ErrUnauthorized, got %v", err)
	}
}

func TestClientFetchToleratesOneBrokenResource(t *testing.T) {
	srv := fakeCompanyServer(t, "rysh_ok", map[string]int{"/phone": http.StatusInternalServerError, "/email": http.StatusNotFound})
	defer srv.Close()
	snap, err := NewClient(srv.URL, "rysh_ok").Fetch(context.Background(), "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("a broken sub-resource must not fail the fetch: %v", err)
	}
	if len(snap.Orders) != 1 || snap.Phone != nil || snap.Email != nil {
		t.Fatalf("partial snapshot: orders=%d phone=%v email=%v", len(snap.Orders), snap.Phone, snap.Email)
	}
	if !snap.Link.Connected || !strings.Contains(snap.Link.LastError, "/phone") {
		t.Fatalf("the 500 must be recorded, the 404 must not: %+v", snap.Link)
	}
}

func TestPickPhoneReadsEitherEnvelope(t *testing.T) {
	if p := pickPhone(map[string]any{"phone": map[string]any{"phone_number": "+44", "status": "pending_review"}}); p == nil || p.Number != "+44" || p.State != "pending_review" {
		t.Fatalf("enveloped: %+v", p)
	}
	if p := pickPhone(map[string]any{"e164": "+1", "state": "active", "status_message": "ok"}); p == nil || p.Number != "+1" || p.Message != "ok" {
		t.Fatalf("bare: %+v", p)
	}
	if p := pickPhone(map[string]any{"phone": nil}); p != nil {
		t.Fatalf("empty: %+v", p)
	}
	var zero msg.CompanyPhone
	_ = zero
}
