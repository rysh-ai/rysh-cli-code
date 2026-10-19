// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type recallClient struct {
	base, key string
	http      *http.Client
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (c recallClient) call(ctx context.Context, method, path string, input, output any) error {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Recall request")
	}
	req.Header.Set("Authorization", c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return errors.New("request to Recall failed; check connectivity and region")
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 408 {
		return recallRejected{status: res.StatusCode, detail: rejectionDetail(res.Body)}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("unexpected HTTP %d from Recall", res.StatusCode)
	} // never expose provider body/key/meeting URL
	if output != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(output)
	}
	return nil
}

// recallRejected is a definite refusal: Recall answered with a 4xx, so the
// request was not carried out. Timeouts (408) and 5xx stay ambiguous.
type recallRejected struct {
	status int
	detail string
}

func (e recallRejected) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("Recall rejected the request (HTTP %d)", e.status)
	}
	return fmt.Sprintf("Recall rejected the request (HTTP %d): %s", e.status, e.detail)
}

// rejectionDetail keeps Recall's validation messages ("field: reason") so a
// schema mismatch is diagnosable, while dropping any string that carries a
// URL: the meeting link and the output-page capability must never be echoed.
func rejectionDetail(body io.Reader) string {
	var v any
	if json.NewDecoder(io.LimitReader(body, 16<<10)).Decode(&v) != nil {
		return ""
	}
	var parts []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(k, t[k])
			}
		case []any:
			for _, x := range t {
				walk(key, x)
			}
		case string:
			if strings.Contains(t, "://") || len(parts) >= 5 {
				return
			}
			if key != "" && key != "detail" && key != "message" {
				t = key + ": " + t
			}
			parts = append(parts, t)
		}
	}
	walk("", v)
	out := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.Join(parts, "; "))
	if len(out) > 300 {
		out = out[:300] + "…"
	}
	return out
}

func createPayload(c Command, token string) map[string]any {
	return map[string]any{
		"meeting_url": c.URL, "bot_name": recallBotName(c),
		"output_media": map[string]any{"camera": map[string]any{"kind": "webpage", "config": map[string]any{"url": c.PublicURL + "/media#" + token}}},
		"recording_config": map[string]any{
			"retention":          nil,
			"transcript":         map[string]any{"provider": map[string]any{"recallai_streaming": map[string]any{"mode": "prioritize_low_latency", "language_code": "en"}}, "diarization": map[string]any{"use_separate_streams_when_available": true}},
			"realtime_endpoints": []any{map[string]any{"type": "webhook", "url": c.PublicURL + "/events", "events": []string{"transcript.data", "participant_events.leave"}}},
		},
		"automatic_leave": map[string]any{"waiting_room_timeout": 300, "noone_joined_timeout": 300, "everyone_left_timeout": map[string]any{"timeout": 30, "activate_after": 1}, "in_call_not_recording_timeout": 300, "in_call_recording_timeout": c.Minutes * 60},
	}
}

// withZoomOBF adds Recall's zoom.obf_token_url when --zoom-obf is on; the
// payload is otherwise the same for every platform.
func withZoomOBF(p map[string]any, z *zoomOBF, publicURL string) map[string]any {
	if z != nil {
		p["zoom"] = map[string]any{"obf_token_url": z.callbackURL(publicURL)}
	}
	return p
}

func verificationKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("RECALL_VERIFICATION_SECRET must be a workspace whsec_ secret")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(b) < 16 {
		return nil, errors.New("invalid RECALL_VERIFICATION_SECRET")
	}
	return b, nil
}

// Verify the raw body before unmarshalling. Accept Recall's current and legacy
// Svix header names, tolerate signing-key rotation, reject stale/future events.
func verifyRequest(h http.Header, body []byte, key []byte, now time.Time) (string, error) {
	get := func(name string) string {
		if v := h.Get("webhook-" + name); v != "" {
			return v
		}
		return h.Get("svix-" + name)
	}
	id, ts, sig := get("id"), get("timestamp"), get("signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || id == "" || len(id) > 256 || now.Sub(time.Unix(sec, 0)) > 5*time.Minute || time.Unix(sec, 0).Sub(now) > 5*time.Minute {
		return "", errors.New("invalid callback")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	for _, v := range strings.Fields(sig) {
		version, b64, ok := strings.Cut(v, ",")
		if !ok || version != "v1" {
			continue
		}
		b, e := base64.StdEncoding.DecodeString(b64)
		if e == nil && hmac.Equal(b, mac.Sum(nil)) {
			return id, nil
		}
	}
	return "", errors.New("invalid callback")
}

func newSpeech(provider, key string) Synthesize {
	return speechWithClient(provider, key, newHTTPClient())
}

// speechRequestError names why a speech request got no response, without
// the reply text or the key: the turn's deadline, or the network.
func speechRequestError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("speech provider timed out")
	}
	return errors.New("speech provider request failed")
}

func speechWithClient(provider, key string, client *http.Client) Synthesize {
	voiced := voiceSpeechWithClient(provider, key, client)
	return func(ctx context.Context, text string) ([]byte, error) { return voiced(ctx, "", text) }
}

// newVoiceSpeech synthesizes in a chosen voice (an attached agent's); ""
// is the provider default. Callers pass voices validated by bindAgent.
func newVoiceSpeech(provider, key string) func(context.Context, string, string) ([]byte, error) {
	return voiceSpeechWithClient(provider, key, newHTTPClient())
}
func voiceSpeechWithClient(provider, key string, client *http.Client) func(context.Context, string, string) ([]byte, error) {
	provider, lang := splitSpeechSpec(provider)
	return func(ctx context.Context, voice, text string) ([]byte, error) {
		if len(text) > 6000 {
			return nil, errors.New("reply exceeds speech limit")
		}
		if provider == ProviderElevenLabs {
			req, err := elevenLabsSpeechRequest(ctx, key, lang, voice, text, "mp3_44100_128", false)
			if err != nil {
				return nil, err
			}
			return readSpeech(ctx, client, req)
		}
		endpoint := "https://api.openai.com/v1/audio/speech"
		openAIVoice := "coral"
		if openAIVoices[voice] {
			openAIVoice = voice
		}
		payload := map[string]any{"model": "gpt-4o-mini-tts", "voice": openAIVoice, "input": text, "response_format": "mp3"}
		auth := "Bearer " + key
		if provider == "deepgram" {
			model := "aura-2-thalia-en"
			if deepgramVoice.MatchString(voice) {
				model = voice
			}
			endpoint = "https://api.deepgram.com/v1/speak?model=" + model + "&encoding=mp3"
			payload = map[string]any{"text": text}
			auth = "Token " + key
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		return readSpeech(ctx, client, req)
	}
}

// readSpeech sends a whole-file speech request and returns the audio.
func readSpeech(ctx context.Context, client *http.Client, req *http.Request) ([]byte, error) {
	res, err := client.Do(req)
	if err != nil {
		return nil, speechRequestError(ctx)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, speechStatusError(res)
	}
	audio, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(audio) == 0 || len(audio) > 8<<20 {
		return nil, errors.New("invalid or oversized speech response")
	}
	return audio, nil
}
