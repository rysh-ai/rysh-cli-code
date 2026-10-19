// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// fakeOgg builds an Ogg/Opus stream of n 20 ms packets (TOC 0xF8: CELT FB
// 20 ms, one frame). The payload is not decodable audio; routing only.
func fakeOgg(t testing.TB, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := oggwriter.NewWith(&buf, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := w.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i * 960)}, Payload: append([]byte{0xF8, byte(i)}, bytes.Repeat([]byte{0x55}, 18)...)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeSink struct {
	mu      sync.Mutex
	samples []media.Sample
}

func (f *fakeSink) WriteSample(s media.Sample, _ *lksdk.SampleWriteOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, s)
	return nil
}
func (f *fakeSink) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.samples) }

func TestPlayOggPacesInRealTime(t *testing.T) {
	sink := &fakeSink{}
	start := time.Now()
	if err := playOgg(context.Background(), sink, fakeOgg(t, 25), nil); err != nil { // 500 ms
		t.Fatal(err)
	}
	if el := time.Since(start); el < 400*time.Millisecond || sink.count() != 25 {
		t.Fatalf("25×20 ms played in %v, %d samples", el, sink.count())
	}
	if d := sink.samples[0].Duration; d != 20*time.Millisecond {
		t.Fatal("sample duration", d)
	}
	if err := playOgg(context.Background(), sink, []byte("ID3 not ogg"), nil); err == nil {
		t.Fatal("accepted non-Ogg speech")
	}
}

func TestDeepgramTranscriberStreamsFinalizesAndEmits(t *testing.T) {
	var auth atomic.Value
	var gotAudio atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
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
			if kind == websocket.BinaryMessage {
				gotAudio.Add(1)
				continue
			}
			var m struct{ Type string }
			_ = json.Unmarshal(p, &m)
			if m.Type == "Finalize" {
				// Two finals: the utterance must be joined, emitted once.
				_ = c.WriteJSON(map[string]any{"type": "Results", "is_final": true, "channel": map[string]any{"alternatives": []any{map[string]any{"transcript": "hey rysh"}}}})
				_ = c.WriteJSON(map[string]any{"type": "Results", "is_final": true, "from_finalize": true, "channel": map[string]any{"alternatives": []any{map[string]any{"transcript": "what is two plus two?"}}}})
			}
			if m.Type == "CloseStream" {
				return
			}
		}
	}))
	defer srv.Close()
	stt := deepgramTranscriber("dg-test-key", func([]string) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }, func() []string { return nil })
	audio := make(chan []byte, 4)
	got := make(chan string, 4)
	done := make(chan error, 1)
	go func() { done <- stt(context.Background(), audio, func(s string) { got <- s }) }()
	audio <- []byte("OggS...")
	audio <- []byte("OggS...")
	select {
	case s := <-got:
		if s != "hey rysh what is two plus two?" {
			t.Fatalf("utterance %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no utterance after the idle Finalize")
	}
	close(audio)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if auth.Load() != "Token dg-test-key" || gotAudio.Load() != 2 {
		t.Fatalf("auth %v audio %d", auth.Load(), gotAudio.Load())
	}
}

func TestOpusSpeechRequestsOggAndRejectsOtherFormats(t *testing.T) {
	body := fakeOgg(t, 1)
	var req map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	speech := opusSpeechWithClient("openai", "k", srv.Client(), srv.URL, srv.URL)
	if audio, err := speech(context.Background(), "", "hi"); err != nil || !bytes.HasPrefix(audio, []byte("OggS")) || req["response_format"] != "opus" {
		t.Fatal(err, req)
	}
	body = []byte("ID3mp3")
	if _, err := speech(context.Background(), "", "hi"); err == nil {
		t.Fatal("accepted MP3 as Ogg/Opus")
	}
}

// fakeRTPTrack delivers a packet every few milliseconds until its read
// deadline is set.
type fakeRTPTrack struct {
	mu     sync.Mutex
	closed bool
	seq    uint16
}

func (f *fakeRTPTrack) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	time.Sleep(2 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, nil, io.EOF
	}
	f.seq++
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: f.seq, Timestamp: uint32(f.seq) * 960}, Payload: []byte{0xF8, 1, 2, 3}}, nil, nil
}
func (f *fakeRTPTrack) SetReadDeadline(time.Time) error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// A failed STT session must not leave the participant unheard: the next
// session gets a fresh Ogg stream (headers first) and keeps transcribing.
func TestTranscribeTrackRestartsAFailedSTTSession(t *testing.T) {
	var sessions atomic.Int32
	heard := make(chan string, 4)
	stt := func(ctx context.Context, audio <-chan []byte, emit func(string)) error {
		n := sessions.Add(1)
		first := <-audio
		if !bytes.HasPrefix(first, []byte("OggS")) || !bytes.Contains(first, []byte("OpusHead")) {
			t.Errorf("session %d did not start with Ogg/Opus headers", n)
		}
		if n == 1 {
			return errors.New("deepgram connect failed")
		}
		emit("hey rysh from session 2")
		for range audio {
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { transcribeTrack(ctx, &fakeRTPTrack{}, stt, func(s string) { heard <- s }); close(done) }()
	select {
	case s := <-heard:
		if s != "hey rysh from session 2" {
			t.Fatal(s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("participant stayed unheard after one STT failure")
	}
	cancel()
	<-done
	if sessions.Load() != 2 {
		t.Fatalf("%d STT sessions", sessions.Load())
	}
}

// Agent voices reach the Opus request (D9: two agent bots in a rysh room
// must not sound alike); unknown or empty voices keep the provider default.
func TestOpusSpeechUsesAgentVoice(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if v, ok := req["voice"].(string); ok {
			got = append(got, v)
		} else {
			got = append(got, r.URL.Query().Get("model")+"|"+r.URL.Query().Get("encoding"))
		}
		_, _ = w.Write(fakeOgg(t, 1))
	}))
	defer srv.Close()
	dg := srv.URL + "/speak?model=aura-2-thalia-en&encoding=opus&container=ogg"
	openai := opusSpeechWithClient("openai", "k", srv.Client(), srv.URL, dg)
	deepgram := opusSpeechStreamWithClient("deepgram", "k", srv.Client(), srv.URL, dg)
	for _, v := range []string{"ash", "", "aura-2-apollo-en"} {
		if _, err := openai(context.Background(), v, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"aura-2-apollo-en", "", "ash"} {
		body, err := deepgram(context.Background(), v, "hi")
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
	}
	want := "ash,coral,coral,aura-2-apollo-en|opus,aura-2-thalia-en|opus,aura-2-thalia-en|opus"
	if strings.Join(got, ",") != want {
		t.Fatalf("voices %v", got)
	}
}
