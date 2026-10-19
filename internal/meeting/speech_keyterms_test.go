// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// E-85: the meeting's agent names (and "Rysh") bias speech-to-text, so a name
// inside a Kurmanji sentence is not lost ("Ez Scribe me" → "Eski rîbma", 048).

func TestSTTKeyterms(t *testing.T) {
	got := sttKeyterms([]string{"scribe", "code-reviewer", "rysh", "Scribe", "", "deep_thinker"})
	want := []string{"Rysh", "scribe", "code reviewer", "deep thinker"}
	if !slices.Equal(got, want) {
		t.Fatalf("sttKeyterms = %q, want %q", got, want)
	}
	if got := sttKeyterms(nil); !slices.Equal(got, []string{"Rysh"}) {
		t.Fatalf("no agents: %q", got)
	}
	many := make([]string, 100)
	for i := range many {
		many[i] = "agent" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if n := len(sttKeyterms(many)); n != maxSTTKeyterms {
		t.Fatalf("capped at %d, got %d", maxSTTKeyterms, n)
	}
}

func TestDeepgramListenURLCarriesTheAgentNamesInEveryLanguage(t *testing.T) {
	for _, lang := range []string{"en", "tr", "kmr"} {
		u, err := url.Parse(deepgramListenURL(lang, sttKeyterms([]string{"scribe", "code-reviewer"})))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if u.Scheme != "wss" || u.Host != "api.deepgram.com" || u.Path != "/v1/listen" {
			t.Errorf("%s: endpoint %s", lang, u)
		}
		if q.Get("model") != "nova-3" || q.Get("language") != lang || q.Get("endpointing") != "300" {
			t.Errorf("%s: query %v", lang, q)
		}
		// Nova-3 takes keyterm for monolingual and multilingual audio, one
		// repeated parameter per term (Deepgram keyterm docs).
		if want := []string{"Rysh", "scribe", "code reviewer"}; !slices.Equal(q["keyterm"], want) {
			t.Errorf("%s: keyterm %q, want %q", lang, q["keyterm"], want)
		}
	}
	if raw := deepgramListenURL("en", []string{"Rysh", "code reviewer"}); !strings.Contains(raw, "keyterm=code+reviewer") {
		t.Errorf("a multi-word term must be URL-encoded: %s", raw)
	}
}

// The agents a bot carries can change while it is in the room (attach /
// detach), so a live Deepgram stream gets a Configure with the new list.
func TestDeepgramTranscriberReconfiguresKeytermsWhenAgentsChange(t *testing.T) {
	var mu sync.Mutex
	var query url.Values
	var configures [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query = r.URL.Query()
		mu.Unlock()
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			kind, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			var m struct {
				Type     string   `json:"type"`
				Keyterms []string `json:"keyterms"`
			}
			_ = json.Unmarshal(p, &m)
			if m.Type == "Configure" {
				mu.Lock()
				configures = append(configures, m.Keyterms)
				mu.Unlock()
			}
			if m.Type == "CloseStream" {
				return
			}
		}
	}))
	defer srv.Close()
	oldWS, oldEvery := deepgramWS, deepgramKeytermEvery
	deepgramWS, deepgramKeytermEvery = "ws"+strings.TrimPrefix(srv.URL, "http"), 10*time.Millisecond
	t.Cleanup(func() { deepgramWS, deepgramKeytermEvery = oldWS, oldEvery })

	var nmu sync.Mutex
	names := []string{"scribe"}
	current := func() []string { nmu.Lock(); defer nmu.Unlock(); return slices.Clone(names) }
	audio := make(chan []byte)
	done := make(chan error, 1)
	go func() {
		done <- NewTranscriber(ProviderDeepgram, "kmr", "dg-key", current)(context.Background(), audio, func(string) {})
	}()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return query != nil })
	mu.Lock()
	if want := []string{"Rysh", "scribe"}; !slices.Equal(query["keyterm"], want) || query.Get("language") != "kmr" {
		t.Errorf("connect query %v, want keyterm %q", query, want)
	}
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(configures) != 0 {
		t.Errorf("Configure sent with no change: %q", configures)
	}
	mu.Unlock()

	nmu.Lock()
	names = []string{"scribe", "planner"}
	nmu.Unlock()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(configures) > 0 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if want := [][]string{{"Rysh", "scribe", "planner"}}; len(configures) != 1 || !slices.Equal(configures[0], want[0]) {
		t.Errorf("configures %q, want exactly %q", configures, want)
	}
	mu.Unlock()
	close(audio)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestElevenLabsTranscriberSendsTheAgentNamesAsKeyterms(t *testing.T) {
	f := &fakeScribe{}
	old := elevenLabsWS
	elevenLabsWS = f.serve(t)
	t.Cleanup(func() { elevenLabsWS = old })
	audio := make(chan []byte, 1024)
	for _, p := range oggOpusPages(t, 0.3) {
		audio <- p
	}
	close(audio)
	names := func() []string { return []string{"scribe", "code-reviewer"} }
	if err := NewTranscriber(ProviderElevenLabs, "kmr", "el-key", names)(context.Background(), audio, func(string) {}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	q, err := url.ParseQuery(f.query)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Rysh", "scribe", "code reviewer"}; !slices.Equal(q["keyterms"], want) || q.Get("language_code") != "kur" {
		t.Fatalf("query %q: keyterms %q, want %q", f.query, q["keyterms"], want)
	}
}

// The LiveKit bot hands the transcriber its live agent list, attach included.
func TestLiveKitBotGivesTheTranscriberItsAgentNames(t *testing.T) {
	h := newJoinRig(t)
	var opts LiveKitDialOptions
	DialLiveKit = func(ctx context.Context, serverURL, token string, o LiveKitDialOptions) (LiveKitRoom, error) {
		opts = o
		return h.room, nil
	}
	if r := h.join(t, existingRoom, h.agent("scribe")); r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if opts.Keyterms == nil {
		t.Fatal("dial options carry no keyterm source")
	}
	if got := sttKeyterms(opts.Keyterms()); !slices.Equal(got, []string{"Rysh", "scribe"}) {
		t.Fatalf("keyterms %q", got)
	}
	s := h.m.active()
	if r := s.attach(h.agent("planner")); r.Error != "" {
		t.Fatalf("attach %+v", r)
	}
	if got := sttKeyterms(opts.Keyterms()); !slices.Equal(got, []string{"Rysh", "scribe", "planner"}) {
		t.Fatalf("after attach: %q", got)
	}
}
