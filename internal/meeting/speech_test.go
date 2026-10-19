// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	opus "github.com/darui3018823/opus"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

func TestSpeechProviderSelector(t *testing.T) {
	var c Command
	c.Defaults()
	if c.TTS != ProviderDeepgram || c.STT != ProviderDeepgram {
		t.Fatalf("defaults tts=%q stt=%q, want deepgram for both (founder 2026-09-29)", c.TTS, c.STT)
	}
	for _, p := range []string{"deepgram", "elevenlabs", "openai"} {
		if !ValidTTS(p) {
			t.Fatalf("tts %s refused", p)
		}
	}
	for _, p := range []string{"", "eleven", "OpenAI", "google"} {
		if ValidTTS(p) {
			t.Fatalf("tts %q accepted", p)
		}
	}
	if !ValidSTT("deepgram") || !ValidSTT("elevenlabs") || ValidSTT("openai") {
		t.Fatal("stt takes deepgram and elevenlabs only")
	}
	for tts, key := range map[string]string{"deepgram": "DEEPGRAM_API_KEY", "elevenlabs": "ELEVENLABS_API_KEY", "openai": "OPENAI_API_KEY"} {
		if SpeechKeyName(tts) != key {
			t.Fatalf("%s key %s", tts, SpeechKeyName(tts))
		}
	}
	if STTKeyName("elevenlabs") != "ELEVENLABS_API_KEY" || STTKeyName("deepgram") != "DEEPGRAM_API_KEY" {
		t.Fatal("stt key names")
	}
	host := Command{Action: "host", TTS: "elevenlabs", STT: "openai", Minutes: 5}
	if err := host.ValidateHost(); err == nil || !strings.Contains(err.Error(), "MEETING_STT") {
		t.Fatalf("host with stt openai: %v", err)
	}
	host.STT = "elevenlabs"
	if err := host.ValidateHost(); err != nil {
		t.Fatal(err)
	}
}

func TestElevenLabsVoicesPerAgent(t *testing.T) {
	ok := func(context.Context, string) (string, error) { return "", nil }
	var attached []meetingAgent
	seen := map[string]bool{}
	for _, name := range []string{"scribe", "coder", "planner", "ada"} {
		b, err := bindAgent(Agent{Name: name, Execute: ok}, ProviderElevenLabs, attached)
		if err != nil {
			t.Fatal(err)
		}
		if !elevenLabsVoice.MatchString(b.Voice) || seen[b.Voice] || b.Voice == elevenLabsDefaultVoice {
			t.Fatalf("%s voice %q: want a distinct ElevenLabs voice id, not rysh's", name, b.Voice)
		}
		seen[b.Voice] = true
		attached = append(attached, b)
	}
	if _, err := bindAgent(Agent{Name: "scribe", Voice: "nova", Execute: ok}, ProviderElevenLabs, nil); err == nil {
		t.Fatal("an OpenAI voice name accepted for elevenlabs")
	}
	if _, err := bindAgent(Agent{Name: "scribe", Voice: "pNInz6obpgDQGcFmaJgB", Execute: ok}, ProviderElevenLabs, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bindAgent(Agent{Name: "scribe", Voice: "pNInz6obpgDQGcFmaJgB", Execute: ok}, ProviderDeepgram, nil); err == nil {
		t.Fatal("an ElevenLabs voice id accepted for deepgram")
	}
	if v := meetingVoice(ProviderElevenLabs, nil); v != defaultVoices[ProviderElevenLabs][0] {
		t.Fatalf("first agent bot voice %q", v)
	}
}

// fakeElevenLabs serves /v1/text-to-speech/{voice}[/stream] and records
// what it was asked.
type elevenLabsCall struct{ path, format, key, model, text string }

func fakeElevenLabsTTS(t *testing.T, status int, body []byte) (*[]elevenLabsCall, *sync.Mutex) {
	var mu sync.Mutex
	var calls []elevenLabsCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Text, ModelID string }
		var raw map[string]string
		_ = json.NewDecoder(r.Body).Decode(&raw)
		b.Text, b.ModelID = raw["text"], raw["model_id"]
		mu.Lock()
		calls = append(calls, elevenLabsCall{r.URL.Path, r.URL.Query().Get("output_format"), r.Header.Get("xi-api-key"), b.ModelID, b.Text})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	old := elevenLabsBase
	elevenLabsBase = srv.URL
	t.Cleanup(func() { elevenLabsBase = old })
	return &calls, &mu
}

func TestElevenLabsSpeechFormats(t *testing.T) {
	calls, mu := fakeElevenLabsTTS(t, 200, []byte("OggS-and-then-some"))
	client := &http.Client{}
	// Recall's media page plays MP3, in the agent's voice or rysh's.
	mp3 := voiceSpeechWithClient(ProviderElevenLabs, "el-key", client)
	if _, err := mp3(context.Background(), "JBFqnCBsd6RMkjVDRZzb", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := mp3(context.Background(), "", "hi"); err != nil {
		t.Fatal(err)
	}
	// A LiveKit track gets Ogg/Opus, whole and streamed.
	whole := opusSpeechWithClient(ProviderElevenLabs, "el-key", client, "http://unused", "http://unused")
	if _, err := whole(context.Background(), "", "hi"); err != nil {
		t.Fatal(err)
	}
	stream := opusSpeechStreamWithClient(ProviderElevenLabs, "el-key", client, "http://unused", "http://unused")
	body, err := stream(context.Background(), "Xb7hH8MSUJpSbSDYk0k2", "streamed")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(body)
	body.Close()
	if !bytes.HasPrefix(b, []byte("OggS")) {
		t.Fatalf("stream body %q", b)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []elevenLabsCall{
		{"/v1/text-to-speech/JBFqnCBsd6RMkjVDRZzb", "mp3_44100_128", "el-key", elevenLabsModel, "hello"},
		{"/v1/text-to-speech/" + elevenLabsDefaultVoice, "mp3_44100_128", "el-key", elevenLabsModel, "hi"},
		{"/v1/text-to-speech/" + elevenLabsDefaultVoice + "/stream", "opus_48000_64", "el-key", elevenLabsModel, "hi"},
		{"/v1/text-to-speech/Xb7hH8MSUJpSbSDYk0k2/stream", "opus_48000_64", "el-key", elevenLabsModel, "streamed"},
	}
	if len(*calls) != len(want) {
		t.Fatalf("calls %+v", *calls)
	}
	for i, c := range *calls {
		if c != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, c, want[i])
		}
	}
}

func TestElevenLabsSpeechErrorsNameReasonNotKey(t *testing.T) {
	fakeElevenLabsTTS(t, 401, []byte(`{"detail":{"status":"invalid_api_key","message":"Invalid API key: sk_secret123"}}`))
	_, err := voiceSpeechWithClient(ProviderElevenLabs, "sk_secret123", &http.Client{})(context.Background(), "", "hi")
	if err == nil || err.Error() != "speech provider returned HTTP 401 (invalid_api_key)" {
		t.Fatalf("err %v", err)
	}
	fakeElevenLabsTTS(t, 402, []byte(`{"detail":"free text with sk_secret123 and spaces"}`))
	_, err = opusSpeechStreamWithClient(ProviderElevenLabs, "sk_secret123", &http.Client{}, "", "")(context.Background(), "", "hi")
	if err == nil || err.Error() != "speech provider returned HTTP 402" {
		t.Fatalf("err %v", err)
	}
	// A 200 that is not Ogg (a format ElevenLabs changed) fails before playback.
	fakeElevenLabsTTS(t, 200, []byte("ID3 mp3 bytes"))
	if _, err := opusSpeechWithClient(ProviderElevenLabs, "k", &http.Client{}, "", "")(context.Background(), "", "hi"); err == nil {
		t.Fatal("non-Ogg accepted for a LiveKit track")
	}
}

// oggOpusPages encodes secs of a 440 Hz tone as transcribeTrack would hand
// it to a transcriber: Opus RTP wrapped in Ogg pages, one Write per page.
func oggOpusPages(t *testing.T, secs float64) [][]byte {
	t.Helper()
	enc, err := opus.NewEncoder(48000, 1, opus.ApplicationVOIP)
	if err != nil {
		t.Fatal(err)
	}
	var pages [][]byte
	w, err := oggwriter.NewWith(pageRecorder{&pages}, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]int16, 960)
	n := int(secs * 50)
	for f := range n {
		for i := range frame {
			frame[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(f*960+i)/48000))
		}
		pkt, err := enc.Encode(frame, 960)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(f), Timestamp: uint32(f * 960)}, Payload: pkt}); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	return pages
}

type pageRecorder struct{ pages *[][]byte }

func (p pageRecorder) Write(b []byte) (int, error) {
	*p.pages = append(*p.pages, append([]byte(nil), b...))
	return len(b), nil
}

// fakeScribe is an ElevenLabs realtime STT endpoint: it checks the key and
// the query, decodes every PCM chunk and answers a commit with a committed
// transcript. reply, when set, is sent instead on the first chunk.
type fakeScribe struct {
	mu      sync.Mutex
	samples int
	peak    int16
	commits int
	query   string
	reply   string
}

func (f *fakeScribe) serve(t *testing.T) string {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "el-key" {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		f.query = r.URL.RawQuery
		f.mu.Unlock()
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"message_type": "session_started"})
		for {
			var m struct {
				Type       string `json:"message_type"`
				Audio      string `json:"audio_base_64"`
				Commit     bool   `json:"commit"`
				SampleRate int    `json:"sample_rate"`
			}
			if conn.ReadJSON(&m) != nil {
				return
			}
			if m.Type != "input_audio_chunk" || m.SampleRate != 16000 {
				t.Errorf("message %+v", m)
				return
			}
			if f.reply != "" {
				_ = conn.WriteJSON(map[string]any{"message_type": f.reply, "error": "Invalid key sk_secret123"})
				return
			}
			pcm, _ := base64.StdEncoding.DecodeString(m.Audio)
			f.mu.Lock()
			f.samples += len(pcm) / 2
			for i := 0; i+1 < len(pcm); i += 2 {
				s := int16(binary.LittleEndian.Uint16(pcm[i:]))
				f.peak = max(f.peak, s)
			}
			if m.Commit {
				f.commits++
			}
			f.mu.Unlock()
			if m.Commit {
				_ = conn.WriteJSON(map[string]any{"message_type": "partial_transcript", "text": "hey"})
				_ = conn.WriteJSON(map[string]any{"message_type": "committed_transcript", "text": " hey rysh what time is it "})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestElevenLabsTranscriberDecodesAndCommits(t *testing.T) {
	f := &fakeScribe{}
	old := elevenLabsWS
	elevenLabsWS = f.serve(t)
	t.Cleanup(func() { elevenLabsWS = old })

	audio := make(chan []byte, 1024)
	for _, p := range oggOpusPages(t, 1) {
		audio <- p
	}
	close(audio)
	var got []string
	err := NewTranscriber(ProviderElevenLabs, "", "el-key", nil)(context.Background(), audio, func(s string) { got = append(got, s) })
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(got) != 1 || got[0] != "hey rysh what time is it" {
		t.Fatalf("emitted %q", got)
	}
	// One second at 16 kHz, less the codec's lookahead; a tone, not silence.
	if f.samples < 14000 || f.samples > 16500 || f.peak < 2000 {
		t.Fatalf("samples %d peak %d", f.samples, f.peak)
	}
	for _, want := range []string{"model_id=scribe_v2_realtime", "audio_format=pcm_16000", "commit_strategy=vad", "language_code=en", "keyterms=Rysh"} {
		if !strings.Contains(f.query, want) {
			t.Fatalf("query %q lacks %s", f.query, want)
		}
	}
}

func TestElevenLabsTranscriberCommitsAfterSilence(t *testing.T) {
	f := &fakeScribe{}
	old := elevenLabsWS
	elevenLabsWS = f.serve(t)
	t.Cleanup(func() { elevenLabsWS = old })

	audio := make(chan []byte, 1024)
	for _, p := range oggOpusPages(t, 0.5) {
		audio <- p
	}
	emitted := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = NewTranscriber(ProviderElevenLabs, "", "el-key", nil)(ctx, audio, func(s string) { emitted <- s })
	}()
	// The sender goes quiet (DTX) but the track stays open: the 700 ms idle
	// commit, not the end of the stream, yields the utterance.
	select {
	case s := <-emitted:
		if s != "hey rysh what time is it" {
			t.Fatalf("emitted %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no utterance after silence")
	}
}

func TestElevenLabsTranscriberErrorsNameTheTypeNotTheText(t *testing.T) {
	f := &fakeScribe{reply: "auth_error"}
	old := elevenLabsWS
	elevenLabsWS = f.serve(t)
	t.Cleanup(func() { elevenLabsWS = old })
	audio := make(chan []byte, 1024)
	for _, p := range oggOpusPages(t, 0.5) {
		audio <- p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := NewTranscriber(ProviderElevenLabs, "", "el-key", nil)(ctx, audio, func(string) {})
	if err == nil || err.Error() != "elevenlabs: auth_error" {
		t.Fatalf("err %v", err)
	}
	// A wrong key is refused at the handshake; the status is named.
	err = NewTranscriber(ProviderElevenLabs, "", "wrong", nil)(ctx, make(chan []byte), func(string) {})
	if err == nil || err.Error() != "elevenlabs connect failed (HTTP 401)" {
		t.Fatalf("err %v", err)
	}
}

func TestDecodeOggOpusSkipsBadPackets(t *testing.T) {
	dec, err := opus.NewDecoder(16000, 1)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, 1920)
	for _, junk := range [][]byte{nil, {0xff}, {0xfc, 0xff, 0xff, 0xff}, bytes.Repeat([]byte{0xff}, 1500)} {
		_ = safeDecode(dec, junk, pcm) // must not panic
	}
	// Not Ogg at all: the session ends quietly and transcribeTrack retries.
	in := make(chan []byte, 1)
	in <- append(bytes.Repeat([]byte("x"), 26), 0) // a whole page header, not "OggS"
	close(in)
	out := make(chan []byte, 4)
	if err := decodeOggOpus(context.Background(), in, out); err == nil {
		t.Fatal("garbage accepted as Ogg")
	}
	if _, open := <-out; open {
		t.Fatal("out left open")
	}
}

func TestDoctorNamesTheChosenProvidersKeys(t *testing.T) {
	r := doctor(Command{TTS: ProviderElevenLabs, STT: ProviderElevenLabs}, Credentials{Speech: "x"})
	if !r.Secrets["ELEVENLABS_API_KEY"] || r.Secrets["DEEPGRAM_API_KEY"] || r.Secrets["OPENAI_API_KEY"] {
		t.Fatalf("secrets %v", r.Secrets)
	}
	r = doctor(Command{TTS: ProviderOpenAI, STT: ProviderDeepgram}, Credentials{Speech: "x"})
	if !r.Secrets["OPENAI_API_KEY"] || r.Secrets["DEEPGRAM_API_KEY"] {
		t.Fatalf("secrets %v", r.Secrets)
	}
	r = doctor(Command{TTS: ProviderOpenAI, STT: ProviderDeepgram}, Credentials{Speech: "x", LiveKit: LiveKitCredentials{STTKey: "dg"}})
	if !r.Secrets["DEEPGRAM_API_KEY"] {
		t.Fatalf("secrets %v", r.Secrets)
	}
}

// E-86: doctor also reports what rysh rooms need — the rysh key for
// host --server (or upstream.api_key) and the LiveKit pair for local host —
// as present/absent only, and the CLI prints every name it reports.
func TestDoctorReportsRoomHostKeys(t *testing.T) {
	names := []string{RyshAPIKeySecret, UpstreamAPIKeyName, LiveKitURLSecret, LiveKitKeySecret, LiveKitSecretSecret}
	r := doctor(Command{}, Credentials{})
	for _, n := range names {
		if present, ok := r.Secrets[n]; !ok || present {
			t.Errorf("absent: %s = %v, %v", n, present, ok)
		}
	}
	r = doctor(Command{}, Credentials{RyshAPIKey: "rysh_x", UpstreamAPIKey: true, LiveKit: LiveKitCredentials{URL: "wss://lk", APIKey: "k", APISecret: "s"}})
	for _, n := range names {
		if !r.Secrets[n] {
			t.Errorf("present: %s not reported", n)
		}
	}
	listed := map[string]bool{}
	for _, n := range DoctorSecretNames {
		listed[n] = true
	}
	r = doctor(Command{ZoomOBF: true, TTS: ProviderOpenAI, STT: ProviderElevenLabs}, Credentials{})
	for n := range r.Secrets {
		if !listed[n] {
			t.Errorf("%s is reported but not in DoctorSecretNames, so the CLI never prints it", n)
		}
	}
	if !strings.Contains(r.Notice, "host --server needs RYSH_API_KEY") || !strings.Contains(r.Notice, "LIVEKIT_URL") || !strings.Contains(r.Notice, "Recall") {
		t.Errorf("notice %q", r.Notice)
	}
}

func TestMeetingLanguage(t *testing.T) {
	if !ValidLang("") || !ValidLang("kmr") || !ValidLang("tr") || ValidLang("KMR") || ValidLang("kurmanji") || ValidLang("k1") {
		t.Fatal("ValidLang")
	}
	// Kurmanji: ElevenLabs speaks and hears; Deepgram speaks English only
	// and has no Kurdish STT; OpenAI TTS takes any text.
	for _, tc := range []struct {
		tts, stt, lang string
		room, ok       bool
	}{
		{"elevenlabs", "elevenlabs", "kmr", true, true},
		{"elevenlabs", "deepgram", "kmr", true, false},
		{"elevenlabs", "deepgram", "kmr", false, true}, // Recall hears for itself
		{"deepgram", "elevenlabs", "kmr", true, false},
		{"openai", "elevenlabs", "kmr", true, true},
		{"elevenlabs", "deepgram", "tr", true, true},
		{"deepgram", "deepgram", "en", true, true},
		{"deepgram", "deepgram", "", true, true},
	} {
		if err := speechLangError(tc.tts, tc.stt, tc.lang, tc.room); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	host := Command{Action: "host", TTS: "elevenlabs", STT: "deepgram", Lang: "kmr", Minutes: 5}
	if err := host.ValidateHost(); err == nil || !strings.Contains(err.Error(), "--stt elevenlabs") {
		t.Fatalf("host: %v", err)
	}
	host.Lang = "Kurmanji"
	if err := host.ValidateHost(); err == nil || !strings.Contains(err.Error(), "MEETING_LANGUAGE") {
		t.Fatalf("host: %v", err)
	}
	if got := replyInLanguage("kmr", "what is the capital of France?"); got != "(Answer in Kurmanji Kurdish, written in the Latin alphabet.) what is the capital of France?" {
		t.Fatalf("reply %q", got)
	}
	if got := replyInLanguage("en", "hi"); got != "hi" {
		t.Fatalf("reply %q", got)
	}
	if sttLanguage(ProviderElevenLabs, "kmr") != "kur" || sttLanguage(ProviderElevenLabs, "") != "en" || sttLanguage(ProviderDeepgram, "tr") != "tr" {
		t.Fatal("stt language")
	}
}

func TestElevenLabsSpeechInAnotherLanguage(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		_, _ = w.Write([]byte("OggS"))
	}))
	t.Cleanup(srv.Close)
	old := elevenLabsBase
	elevenLabsBase = srv.URL
	t.Cleanup(func() { elevenLabsBase = old })

	c := &http.Client{}
	for _, spec := range []string{speechSpec(ProviderElevenLabs, "kmr"), speechSpec(ProviderElevenLabs, "tr"), speechSpec(ProviderElevenLabs, "en")} {
		if _, err := opusSpeechWithClient(spec, "k", c, "", "")(context.Background(), "", "Silav"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := voiceSpeechWithClient(speechSpec(ProviderElevenLabs, "kmr"), "k", c)(context.Background(), "", "Silav"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		`eleven_v4_turbo <nil>`, // Kurmanji: no ISO 639-1 code; the model reads the text
		`eleven_v4_turbo tr`,
		`eleven_flash_v2_5 <nil>`,
		`eleven_v4_turbo <nil>`,
	}
	for i, b := range bodies {
		if got := fmt.Sprint(b["model_id"], " ", b["language_code"]); got != want[i] {
			t.Fatalf("request %d: %s, want %s", i, got, want[i])
		}
	}
	if speechSpec(ProviderDeepgram, "") != "deepgram" || speechSpec(ProviderElevenLabs, "en") != "elevenlabs" {
		t.Fatal("English keeps the bare provider")
	}
}

func TestElevenLabsTranscriberListensForKurdish(t *testing.T) {
	f := &fakeScribe{}
	old := elevenLabsWS
	elevenLabsWS = f.serve(t)
	t.Cleanup(func() { elevenLabsWS = old })
	audio := make(chan []byte, 1024)
	for _, p := range oggOpusPages(t, 0.3) {
		audio <- p
	}
	close(audio)
	if err := NewTranscriber(ProviderElevenLabs, "kmr", "el-key", nil)(context.Background(), audio, func(string) {}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.query, "language_code=kur") {
		t.Fatalf("query %q", f.query)
	}
}
