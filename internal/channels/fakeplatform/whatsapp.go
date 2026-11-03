// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// WhatsAppSend is one text POST /{version}/{phone_number_id}/messages.
type WhatsAppSend struct {
	To   string
	Text string
}

// WhatsApp is a fake WhatsApp Cloud API (Graph messages endpoint) plus a
// webhook sender: DeliverDM signs a webhook body with the app secret
// (X-Hub-Signature-256) and POSTs it to the adapter's webhook, as Meta does.
type WhatsApp struct {
	Token   string
	Secret  string
	PhoneID string

	srv *httptest.Server

	mu     sync.Mutex
	sends  []WhatsAppSend
	nextID int
}

// NewWhatsApp starts a fake Graph API on 127.0.0.1 accepting only token for
// phone number id phoneID.
func NewWhatsApp(token, secret, phoneID string) *WhatsApp {
	f := &WhatsApp{Token: token, Secret: secret, PhoneID: phoneID, nextID: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the API root to put in ChannelConfig.APIBase.
func (f *WhatsApp) URL() string { return f.srv.URL }

// Close stops the server.
func (f *WhatsApp) Close() { f.srv.Close() }

// Sends returns a copy of every text sent so far.
func (f *WhatsApp) Sends() []WhatsAppSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]WhatsAppSend(nil), f.sends...)
}

func (f *WhatsApp) serve(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"error":{"message":"Invalid OAuth access token","code":190}}`))
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/"+f.PhoneID+"/messages") {
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"error":{"message":"Unknown path","code":100}}`))
		return
	}
	var body struct {
		To   string `json:"to"`
		Type string `json:"type"`
		Text struct {
			Body string `json:"body"`
		} `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	if body.Type == "text" {
		f.sends = append(f.sends, WhatsAppSend{To: body.To, Text: body.Text.Body})
	}
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	fmt.Fprintf(rw, `{"messaging_product":"whatsapp","contacts":[{"input":%q,"wa_id":%q}],"messages":[{"id":"wamid.fake%d"}]}`,
		body.To, body.To, id)
}

// DeliverDM POSTs a signed text-message webhook from wa_id from to hook.
func (f *WhatsApp) DeliverDM(hook, from, text string) {
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	payload := map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": "waba-fake",
			"changes": []any{map[string]any{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata":          map[string]string{"display_phone_number": "15550000000", "phone_number_id": f.PhoneID},
					"contacts":          []any{map[string]any{"wa_id": from, "profile": map[string]string{"name": "u" + from}}},
					"messages": []any{map[string]any{
						"from": from, "id": fmt.Sprintf("wamid.in%d", id),
						"timestamp": fmt.Sprint(time.Now().Unix()), "type": "text",
						"text": map[string]string{"body": text},
					}},
				},
			}},
		}},
	}
	raw, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(f.Secret))
	mac.Write(raw)
	req, err := http.NewRequest(http.MethodPost, hook, bytes.NewReader(raw))
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: whatsapp webhook request: %v", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: whatsapp webhook delivery: %v", err))
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("fakeplatform: whatsapp webhook answered HTTP %d", resp.StatusCode))
	}
}
