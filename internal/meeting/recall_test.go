// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("01234567890123456789012345678901")

func signed(id string, body []byte, now time.Time) http.Header {
	h := http.Header{}
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, testKey)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h
}
func TestVerifyRecallRejectsForgeryReplayAndSupportsRotation(t *testing.T) {
	now := time.Now()
	body := []byte(`{"event":"transcript.data"}`)
	h := signed("event-1", body, now)
	if _, err := verifyRequest(h, body, testKey, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body []byte
		at   time.Time
	}{{[]byte(`{}`), now}, {body, now.Add(6 * time.Minute)}, {body, now.Add(-6 * time.Minute)}} {
		if _, err := verifyRequest(h, tc.body, testKey, tc.at); err == nil {
			t.Fatal("accepted forged or stale event")
		}
	}
	h.Set("webhook-signature", "v1,invalid "+h.Get("webhook-signature"))
	if _, err := verifyRequest(h, body, testKey, now); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"id", "timestamp", "signature"} {
		h.Set("svix-"+n, h.Get("webhook-"+n))
		h.Del("webhook-" + n)
	}
	if _, err := verifyRequest(h, body, testKey, now); err != nil {
		t.Fatal(err)
	}
}
func TestRecallPayloadZeroRetentionAndBoundedLifetime(t *testing.T) {
	c := Command{URL: "https://meet.google.com/abc-defg-hij", PublicURL: "https://meeting.example", Consent: true}
	c.Defaults()
	p := createPayload(c, "capability")
	b, _ := json.Marshal(p)
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	rc := decoded["recording_config"].(map[string]any)
	v, ok := rc["retention"]
	if !ok || v != nil {
		t.Fatal("retention must be explicit null")
	}
	lifetime := decoded["automatic_leave"].(map[string]any)
	if lifetime["in_call_recording_timeout"] != float64(3600) {
		t.Fatal(lifetime)
	}
	left, ok := lifetime["everyone_left_timeout"].(map[string]any)
	if !ok {
		t.Fatal("incorrect Recall timeout schema")
	}
	// Recall answers 400 "Ensure this value is greater than or equal to 1" for 0.
	if a, _ := left["activate_after"].(float64); a < 1 {
		t.Fatal("everyone_left_timeout.activate_after must be >= 1:", left)
	}
	if strings.Contains(string(b), "automatic_audio_output") || !strings.Contains(string(b), "/media#capability") {
		t.Fatal(string(b))
	}
}
func TestRecallErrorsAndRedirectsDoNotLeak(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed credential redirect") }))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" {
			t.Error("missing key")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer src.Close()
	err := (recallClient{src.URL, "test-key", newHTTPClient()}).call(context.Background(), "POST", "/bot/", map[string]string{}, nil)
	if err == nil || strings.Contains(err.Error(), "test-key") {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSpeechProviderContracts(t *testing.T) {
	for _, provider := range []string{"openai", "deepgram"} {
		t.Run(provider, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var p map[string]any
				_ = json.NewDecoder(r.Body).Decode(&p)
				if provider == "openai" {
					if r.URL.Host != "api.openai.com" || p["input"] != "verified answer" || p["response_format"] != "mp3" || r.Header.Get("Authorization") != "Bearer key" {
						t.Fatal("bad OpenAI request")
					}
				} else {
					if r.URL.Host != "api.deepgram.com" || p["text"] != "verified answer" || r.Header.Get("Authorization") != "Token key" {
						t.Fatal("bad Deepgram request")
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("MP3")), Header: http.Header{}}, nil
			})}
			b, err := speechWithClient(provider, "key", client)(context.Background(), "verified answer")
			if err != nil || string(b) != "MP3" {
				t.Fatal(err)
			}
		})
	}
}

// An attached agent's voice reaches the provider; anything unvalidated falls
// back to the default instead of being forwarded.
func TestSpeechVoiceSelection(t *testing.T) {
	var got string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if r.URL.Host == "api.deepgram.com" {
			got = r.URL.Query().Get("model")
		} else {
			got, _ = p["voice"].(string)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("MP3")), Header: http.Header{}}, nil
	})}
	for _, c := range []struct{ provider, voice, want string }{
		{"openai", "nova", "nova"},
		{"openai", "", "coral"},
		{"openai", "aura-2-apollo-en", "coral"},
		{"deepgram", "aura-2-apollo-en", "aura-2-apollo-en"},
		{"deepgram", "nova", "aura-2-thalia-en"},
		{"deepgram", "aura-2-x&y-en", "aura-2-thalia-en"},
	} {
		if _, err := voiceSpeechWithClient(c.provider, "key", client)(context.Background(), c.voice, "hi"); err != nil || got != c.want {
			t.Errorf("%s %q: sent %q, want %q (%v)", c.provider, c.voice, got, c.want, err)
		}
	}
}
