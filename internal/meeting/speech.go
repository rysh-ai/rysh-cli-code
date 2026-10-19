// SPDX-License-Identifier: Apache-2.0

package meeting

// Speech providers (design 048): one selector for speech-to-text (STT, how a
// rysh room's participants are heard) and one for text-to-speech (TTS, how
// replies are spoken). Deepgram is the default for both (founder,
// 2026-09-29); ElevenLabs is the alternative for both, OpenAI stays a TTS.
// Recall bots are heard by Recall's own streaming transcription, so STT
// applies to rysh rooms only.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	opus "github.com/darui3018823/opus"
	"github.com/gorilla/websocket"
	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"
)

// Provider names, as typed in --tts/--stt and MEETING_TTS/MEETING_STT.
const (
	ProviderDeepgram   = "deepgram"
	ProviderElevenLabs = "elevenlabs"
	ProviderOpenAI     = "openai"

	DefaultTTS = ProviderDeepgram
	DefaultSTT = ProviderDeepgram
)

// ValidTTS and ValidSTT report whether p is a provider the selector takes.
func ValidTTS(p string) bool {
	return p == ProviderDeepgram || p == ProviderElevenLabs || p == ProviderOpenAI
}
func ValidSTT(p string) bool { return p == ProviderDeepgram || p == ProviderElevenLabs }

// SpeechKeyName is the session secret a TTS provider needs.
func SpeechKeyName(tts string) string {
	switch tts {
	case ProviderOpenAI:
		return "OPENAI_API_KEY"
	case ProviderElevenLabs:
		return "ELEVENLABS_API_KEY"
	}
	return "DEEPGRAM_API_KEY"
}

// STTKeyName is the session secret an STT provider needs.
func STTKeyName(stt string) string {
	if stt == ProviderElevenLabs {
		return "ELEVENLABS_API_KEY"
	}
	return "DEEPGRAM_API_KEY"
}

// roomSpeechKeysError names the first provider key a rysh room bot is
// missing: the --tts key (unless it is the listen-only reporter) and the
// --stt key, which every bot in a room hears with. Host checks it before
// creating anything, so a missing key never leaves a room behind (E-86).
func roomSpeechKeysError(c Command, keys Credentials) error {
	if keys.Speech == "" && c.report == nil {
		return fmt.Errorf("set %s in this session's secrets (text-to-speech, --tts %s)", SpeechKeyName(c.TTS), ttsName(c.TTS))
	}
	if keys.LiveKit.STTKey == "" {
		return fmt.Errorf("set %s in this session's secrets (speech-to-text for rysh rooms, --stt %s)", STTKeyName(c.STT), sttName(c.STT))
	}
	return nil
}

func ttsName(p string) string {
	if p == "" {
		return DefaultTTS
	}
	return p
}

func sttName(p string) string {
	if p == "" {
		return DefaultSTT
	}
	return p
}

// ElevenLabs voices are voice ids (20 letters and digits, as the voice
// library shows them). The default and the per-agent defaults are premade
// voices every account has: Rachel for rysh; George, Sarah, Brian and Alice
// for agents (D9: each agent bot a distinct voice).
var elevenLabsVoice = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)

const elevenLabsDefaultVoice = "21m00Tcm4TlvDq8ikWAM"

// elevenLabsModel is ElevenLabs' low-latency TTS model (Flash v2.5).
const elevenLabsModel = "eleven_flash_v2_5"

// elevenLabsBase and elevenLabsWS are the API roots; tests point them at
// httptest servers.
var (
	elevenLabsBase = "https://api.elevenlabs.io"
	elevenLabsWS   = "wss://api.elevenlabs.io"
)

// validVoice reports whether voice is one the tts provider can speak in.
func validVoice(tts, voice string) bool {
	switch tts {
	case ProviderDeepgram:
		return deepgramVoice.MatchString(voice)
	case ProviderElevenLabs:
		return elevenLabsVoice.MatchString(voice)
	}
	return openAIVoices[voice]
}

// elevenLabsSpeechRequest asks ElevenLabs for text in voice ("" = the
// default) as format: mp3_44100_128 for Recall's media page, opus_48000_64
// for a LiveKit track. stream selects the endpoint that sends audio as it is
// synthesized.
func elevenLabsSpeechRequest(ctx context.Context, key, lang, voice, text, format string, stream bool) (*http.Request, error) {
	if !elevenLabsVoice.MatchString(voice) {
		voice = elevenLabsDefaultVoice
	}
	path := "/v1/text-to-speech/" + voice
	if stream {
		path += "/stream"
	}
	payload := map[string]any{"text": text, "model_id": elevenLabsModel}
	if !english(lang) {
		payload["model_id"] = elevenLabsMultilingualModel
		if len(lang) == 2 {
			// ISO 639-1 only; ElevenLabs ignores one the model lacks.
			payload["language_code"] = lang
		}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", elevenLabsBase+path+"?"+url.Values{"output_format": {format}}.Encode(), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// speechStatusError names a non-200 speech response: the status and, when
// the provider sent one, its short machine-readable reason (ElevenLabs'
// detail.status such as "invalid_api_key" or "quota_exceeded", Deepgram's
// err_code). Never the key, never free text from the body.
func speechStatusError(res *http.Response) error {
	var body struct {
		Detail  json.RawMessage `json:"detail"`
		ErrCode string          `json:"err_code"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body)
	reason := body.ErrCode
	var detail struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body.Detail, &detail) == nil && detail.Status != "" {
		reason = detail.Status
	}
	if speechReason.MatchString(reason) {
		return fmt.Errorf("speech provider returned HTTP %d (%s)", res.StatusCode, reason)
	}
	return fmt.Errorf("speech provider returned HTTP %d", res.StatusCode)
}

var speechReason = regexp.MustCompile(`^[A-Za-z_]{1,40}$`)

// NewTranscriber is the rysh-room STT for provider in lang ("" = English),
// key its API key. names, when set, is the bot's current agent names: they
// and "Rysh" bias recognition (sttKeyterms, E-85), read when each participant
// stream connects. Deepgram also follows a change mid-stream.
func NewTranscriber(provider, lang, key string, names func() []string) Transcriber {
	if names == nil {
		names = func() []string { return nil }
	}
	if provider == ProviderElevenLabs {
		return elevenLabsTranscriber(key, sttLanguage(provider, lang), names)
	}
	return deepgramTranscriber(key, func(terms []string) string { return deepgramListenURL(sttLanguage(provider, lang), terms) }, names)
}

// maxSTTKeyterms bounds the bias list: Deepgram allows 500 tokens and
// advises 20–50 terms; a meeting has at most MaxAgentBots agent bots.
const maxSTTKeyterms = 50

// sttKeyterms is what STT is biased towards: "Rysh" (the wake word), then
// each agent name as it is spoken ("code-reviewer" → "code reviewer"),
// without duplicates, at most maxSTTKeyterms. Agent names are lowercase
// letter words (ValidAgentName), so no term needs more than URL encoding.
func sttKeyterms(names []string) []string {
	terms := []string{"Rysh"}
	seen := map[string]bool{"rysh": true}
	for _, n := range names {
		term := strings.Join(strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == ' ' }), " ")
		if term == "" || seen[strings.ToLower(term)] || len(terms) >= maxSTTKeyterms {
			continue
		}
		seen[strings.ToLower(term)] = true
		terms = append(terms, term)
	}
	return terms
}

// deepgramWS is Deepgram's streaming root; tests point it at a fake.
var deepgramWS = "wss://api.deepgram.com"

// deepgramListenURL is the nova-3 live endpoint for lang, biased towards
// terms. Nova-3 takes keyterm for monolingual and multilingual audio, one
// repeated parameter per term (developers.deepgram.com/docs/keyterm), so
// it is sent in every language, not only English.
func deepgramListenURL(lang string, terms []string) string {
	q := url.Values{"model": {"nova-3"}, "language": {lang}, "smart_format": {"true"}, "punctuate": {"true"}, "endpointing": {"300"}, "keyterm": terms}
	return deepgramWS + "/v1/listen?" + q.Encode()
}

// ElevenLabsTranscriber streams a participant to ElevenLabs Scribe v2
// Realtime. The endpoint takes PCM only, so the track's Ogg/Opus is decoded
// here (pure Go, 16 kHz mono) — the one place the meeting path decodes
// audio. ElevenLabs' VAD commits each utterance; like Deepgram's Finalize,
// 700 ms without audio (a sender in DTX silence) commits what is pending.
func ElevenLabsTranscriber(key string) Transcriber {
	return NewTranscriber(ProviderElevenLabs, "", key, nil)
}

// elevenLabsTranscriber sends the keyterms when each stream connects:
// Scribe v2 Realtime takes `keyterms` as a query array and documents no
// message to change it mid-session (ElevenLabs API reference, realtime STT).
func elevenLabsTranscriber(key, lang string, names func() []string) Transcriber {
	return func(ctx context.Context, audio <-chan []byte, emit func(string)) error {
		q := url.Values{"model_id": {"scribe_v2_realtime"}, "audio_format": {"pcm_16000"}, "commit_strategy": {"vad"}, "language_code": {lang}, "keyterms": sttKeyterms(names())}
		return elevenLabsTranscribe(ctx, key, elevenLabsWS+"/v1/speech-to-text/realtime?"+q.Encode(), audio, emit)
	}
}

func elevenLabsTranscribe(ctx context.Context, key, endpoint string, audio <-chan []byte, emit func(string)) error {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, res, err := d.DialContext(ctx, endpoint, http.Header{"xi-api-key": []string{key}})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		if res != nil {
			return fmt.Errorf("elevenlabs connect failed (HTTP %d)", res.StatusCode)
		}
		return errors.New("elevenlabs connect failed")
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	readErr := make(chan error, 1)
	committed := make(chan struct{}, 1)
	go func() {
		for {
			var m struct {
				Type string `json:"message_type"`
				Text string `json:"text"`
			}
			if conn.ReadJSON(&m) != nil {
				readErr <- errors.New("elevenlabs stream closed")
				return
			}
			switch {
			case m.Type == "committed_transcript":
				if t := strings.TrimSpace(m.Text); t != "" {
					emit(t)
				}
				select {
				case committed <- struct{}{}:
				default:
				}
			case m.Type == "auth_error", m.Type == "quota_exceeded", m.Type == "rate_limited", m.Type == "session_time_limit_exceeded", m.Type == "unaccepted_terms", m.Type == "resource_exhausted":
				// The type names why; the error text is not repeated.
				readErr <- errors.New("elevenlabs: " + m.Type)
				return
			}
		}
	}()
	var mu sync.Mutex
	send := func(pcm []byte, commit bool) error {
		mu.Lock()
		defer mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(map[string]any{"message_type": "input_audio_chunk", "audio_base_64": base64.StdEncoding.EncodeToString(pcm), "commit": commit, "sample_rate": 16000})
	}
	dctx, stop := context.WithCancel(ctx)
	defer stop()
	pcm := make(chan []byte, 64)
	decodeErr := make(chan error, 1)
	go func() { decodeErr <- decodeOggOpus(dctx, audio, pcm) }()
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	var buf []byte
	pending := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case p, ok := <-pcm:
			if !ok {
				if len(buf) > 0 || pending {
					select {
					case <-committed: // an earlier commit's
					default:
					}
					if send(buf, true) == nil {
						// Wait briefly for the last committed transcript.
						select {
						case <-committed:
						case <-readErr:
						case <-time.After(3 * time.Second):
						}
					}
				}
				if err := <-decodeErr; err != nil {
					return err
				}
				return nil
			}
			buf = append(buf, p...)
			// ~100 ms chunks (16 kHz × 2 bytes): fewer, larger messages.
			if len(buf) >= 3200 {
				if send(buf, false) != nil {
					return errors.New("elevenlabs send failed")
				}
				buf, pending = nil, true
			}
			idle.Reset(700 * time.Millisecond)
		case <-idle.C:
			if len(buf) > 0 || pending {
				_ = send(buf, true)
				buf, pending = nil, false
			}
		}
	}
}

// decodeOggOpus turns the Ogg/Opus pages on in (as transcribeTrack writes
// them) into 16 kHz mono little-endian PCM on out, closing out when in
// closes. A packet the decoder rejects — or panics on: it is participant
// audio from the network — is skipped, not fatal.
func decodeOggOpus(ctx context.Context, in <-chan []byte, out chan<- []byte) error {
	defer close(out)
	pr, pw := io.Pipe()
	go func() {
		for p := range in {
			if _, err := pw.Write(p); err != nil {
				break
			}
		}
		_ = pw.Close()
		for range in { // drain so the RTP reader never blocks on us
		}
	}()
	defer pr.Close()
	r, _, err := oggreader.NewOggReader(pr)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return errors.New("elevenlabs: unreadable Ogg/Opus audio")
	}
	dec, err := opus.NewDecoder(16000, 1)
	if err != nil {
		return errors.New("elevenlabs: opus decoder unavailable")
	}
	frame := make([]int16, 16000*120/1000) // the longest Opus packet, 120 ms
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			return nil
		}
		n := safeDecode(dec, pkt, frame)
		if n <= 0 {
			continue
		}
		b := make([]byte, 2*n)
		for i, s := range frame[:n] {
			binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
		}
		select {
		case out <- b:
		case <-ctx.Done():
			return nil
		}
	}
}

func safeDecode(dec *opus.Decoder, pkt []byte, pcm []int16) (n int) {
	defer func() {
		if recover() != nil {
			n = 0
		}
	}()
	n, err := dec.Decode(pkt, pcm)
	if err != nil {
		return 0
	}
	return n
}

// Meeting language (design 048 §Languages): --lang / MEETING_LANGUAGE, an ISO
// 639 code; "" or "en" is English, unchanged. Another language makes the
// agent answer in it, ElevenLabs speak it with its multilingual model and the
// STT listen for it. Kurmanji is not in ElevenLabs' language list, but
// eleven_v4_turbo reads Latin-script Kurmanji (founder, by ear, 2026-09-30).
var meetingLang = regexp.MustCompile(`^[a-z]{2,3}$`)

// ValidLang reports whether l is a language code --lang takes ("" = English).
func ValidLang(l string) bool { return l == "" || meetingLang.MatchString(l) }

func english(l string) bool { return l == "" || l == "en" }

func kurdish(l string) bool { return l == "ku" || l == "kmr" || l == "ckb" || l == "kur" }

// elevenLabsMultilingualModel speaks every language but English.
const elevenLabsMultilingualModel = "eleven_v4_turbo"

// langName names l for the agent's reply instruction.
func langName(l string) string {
	switch l {
	case "ku", "kmr", "kur":
		return "Kurmanji Kurdish, written in the Latin alphabet"
	case "ckb":
		return "Sorani Kurdish"
	case "tr":
		return "Turkish"
	}
	return "the language with ISO 639 code " + l
}

// replyInLanguage prefixes a request with the answer language, so every agent
// (rysh or an attached one) replies in it; English requests are unchanged.
func replyInLanguage(lang, text string) string {
	if english(lang) {
		return text
	}
	return "(Answer in " + langName(lang) + ".) " + text
}

// speechLangError refuses a language a chosen provider cannot handle. room
// is true for rysh rooms, where the STT provider matters.
func speechLangError(tts, stt, lang string, room bool) error {
	if english(lang) {
		return nil
	}
	if tts == ProviderDeepgram {
		return fmt.Errorf("Deepgram voices here speak English; for --lang %s use --tts elevenlabs (or openai)", lang)
	}
	if room && stt == ProviderDeepgram && kurdish(lang) {
		return errors.New("Deepgram does not transcribe Kurdish; use --stt elevenlabs")
	}
	return nil
}

// speechSpec carries a TTS provider and its language through the speech
// constructors, which take one provider string: "elevenlabs@kmr".
func speechSpec(tts, lang string) string {
	if english(lang) {
		return tts
	}
	return tts + "@" + lang
}

func splitSpeechSpec(spec string) (provider, lang string) {
	provider, lang, _ = strings.Cut(spec, "@")
	return provider, lang
}

// sttLanguage is the language code the STT provider is asked for.
func sttLanguage(provider, lang string) string {
	switch {
	case english(lang):
		return "en"
	case provider == ProviderElevenLabs && kurdish(lang):
		return "kur" // Scribe lists Kurdish as one language
	}
	return lang
}
