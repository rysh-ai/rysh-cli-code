// SPDX-License-Identifier: Apache-2.0

package meeting

// LiveKit room client (E29 LK-1, design 042): the WebRTC side of the LK-2
// transport. The bot joins a rysh-native LiveKit room, hears each participant
// on their own audio track (identity from the signed token), and speaks on
// its own track. Wake phrase, allowlist, hands-free turns, meetingexec and
// TTS are the Service's, unchanged.
//
// No cgo: subscribed Opus RTP is wrapped in Ogg (pion oggwriter) and streamed
// to an STT that accepts containerized Opus; TTS is asked for Ogg/Opus and its
// packets are published as samples. Nothing is decoded or re-encoded locally.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

func init() {
	// Register the WebRTC room client with the LK-2 transport seam.
	DialLiveKit = dialLiveKitRoom
	LiveKitSpeech = func(tts, key string) Synthesize { return Synthesize(NewOpusSpeech(tts, key)) }
	LiveKitSpeechStream = NewOpusSpeechStream
	LiveKitVoiceSpeech = NewOpusVoiceSpeech
	LiveKitVoiceSpeechStream = NewOpusVoiceSpeechStream
}

// LiveKitConfig names a room and the API key pair that mints tokens for it;
// used by the synthetic participant (LiveKitProbe) and the dev harness.
type LiveKitConfig struct {
	URL       string // ws(s):// signalling URL
	APIKey    string
	APISecret string
	Room      string
}

// Transcriber consumes one participant's Ogg/Opus byte stream and calls emit
// for every final utterance until audio closes or ctx ends.
type Transcriber func(ctx context.Context, audio <-chan []byte, emit func(text string)) error

// OpusSpeech synthesizes text to Ogg/Opus.
type OpusSpeech func(ctx context.Context, text string) ([]byte, error)

type sampleWriter interface {
	WriteSample(media.Sample, *lksdk.SampleWriteOptions) error
}

// liveKitConn is the bot's WebRTC connection to one room (LiveKitRoom): it
// joins with the token the transport minted, transcribes every other
// non-agent participant's own audio track, and plays replies on one track.
type liveKitConn struct {
	room   *lksdk.Room
	out    sampleWriter
	stt    Transcriber
	events chan LiveKitEvent
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	// evMu orders emit's send against shutdown's close: SDK callbacks
	// (participant disconnected) run on their own goroutines and can fire
	// while the connection is being closed.
	evMu     sync.Mutex
	evClosed bool
}

func dialLiveKitRoom(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error) {
	if opts.STTKey == "" {
		return nil, fmt.Errorf("set %s in this session's secrets (speech-to-text for rysh rooms)", STTKeyName(opts.STT))
	}
	return connectLiveKit(ctx, serverURL, token, NewTranscriber(opts.STT, opts.Lang, opts.STTKey, opts.Keyterms))
}

// connectLiveKit joins a room with token and publishes the bot's audio track.
func connectLiveKit(_ context.Context, serverURL, token string, stt Transcriber) (*liveKitConn, error) {
	cctx, cancel := context.WithCancel(context.Background())
	c := &liveKitConn{stt: stt, events: make(chan LiveKitEvent, 64), ctx: cctx, cancel: cancel}
	cb := lksdk.NewRoomCallback()
	cb.ParticipantCallback.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		// Other agents are not listened to: no bot-to-bot loops.
		if track.Kind() != webrtc.RTPCodecTypeAudio || rp.Kind() == lksdk.ParticipantAgent {
			return
		}
		identity, name := rp.Identity(), rp.Name()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			transcribeTrack(c.ctx, track, c.stt, func(text string) {
				c.emit(LiveKitEvent{Kind: LiveKitUtterance, Identity: identity, Name: name, Text: text})
			})
		}()
	}
	cb.OnParticipantDisconnected = func(rp *lksdk.RemoteParticipant) {
		c.emit(LiveKitEvent{Kind: LiveKitLeft, Identity: rp.Identity(), Name: rp.Name()})
	}
	cb.OnDisconnected = func() { go c.shutdown() }
	room, err := lksdk.ConnectToRoomWithToken(serverURL, token, cb)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("join livekit room: %w", err)
	}
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err == nil {
		_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "rysh-voice", Source: livekit.TrackSource_MICROPHONE})
	}
	if err != nil {
		room.Disconnect()
		cancel()
		return nil, fmt.Errorf("publish bot audio: %w", err)
	}
	c.room, c.out = room, track
	return c, nil
}

// emit never blocks the WebRTC callbacks; a stalled consumer loses events.
func (c *liveKitConn) emit(ev LiveKitEvent) {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	if c.evClosed {
		return
	}
	select {
	case c.events <- ev:
	default:
	}
}

func (c *liveKitConn) Events() <-chan LiveKitEvent { return c.events }

// PublishData sends payload to everyone in the room on topic, reliably (the
// app's MeetingRoom reads the bot's wall from "rysh.wall").
func (c *liveKitConn) PublishData(topic string, payload []byte) error {
	return c.room.LocalParticipant.PublishDataPacket(lksdk.UserData(payload), lksdk.WithDataPublishTopic(topic), lksdk.WithDataPublishReliable(true))
}

// Play publishes one Ogg/Opus reply (LiveKitSpeech's format) on the bot's
// track, paced in real time; cancelling ctx stops it mid-reply.
func (c *liveKitConn) Play(ctx context.Context, audio []byte) error {
	return playOgg(ctx, c.out, audio, nil)
}

// PlayStream publishes a reply while it is still being synthesized.
// Cancelling ctx closes the stream, which abandons the provider request.
func (c *liveKitConn) PlayStream(ctx context.Context, audio io.ReadCloser) error {
	defer audio.Close()
	stop := context.AfterFunc(ctx, func() { _ = audio.Close() })
	defer stop()
	return playOggReader(ctx, c.out, audio, nil)
}

func (c *liveKitConn) Close() error {
	c.room.Disconnect()
	c.shutdown()
	return nil
}

func (c *liveKitConn) shutdown() {
	c.once.Do(func() {
		c.cancel()
		c.wg.Wait()
		c.evMu.Lock()
		c.evClosed = true
		close(c.events)
		c.evMu.Unlock()
	})
}

// rtpTrack is what transcribeTrack reads; *webrtc.TrackRemote implements it.
type rtpTrack interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
	SetReadDeadline(time.Time) error
}

// transcribeTrack streams one participant track to STT: Opus RTP is wrapped
// in Ogg, never decoded. A session that ends while the track is still live
// (connect timeout, provider drop) is replaced — with a fresh Ogg stream,
// since each session needs its own headers — so one STT error does not leave
// the participant unheard for the rest of the meeting. Same loop as
// rysh-server internal/lkagent.
func transcribeTrack(ctx context.Context, track rtpTrack, stt Transcriber, emit func(string)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); _ = track.SetReadDeadline(time.Now()) }()
	backoff := time.Second
	for ctx.Err() == nil {
		began := time.Now()
		audio := make(chan []byte, 1024)
		done := make(chan struct{})
		go func() { defer close(done); _ = stt(ctx, audio, emit) }()
		trackOver := true
		if w, err := oggwriter.NewWith(chanWriter{audio}, 48000, 1); err == nil {
			trackOver = false
		read:
			for {
				select {
				case <-done: // the STT session ended; the track goes on
					break read
				default:
				}
				pkt, _, err := track.ReadRTP()
				if err != nil {
					trackOver = true
					break
				}
				if len(pkt.Payload) == 0 {
					continue
				}
				if w.WriteRTP(pkt) != nil {
					trackOver = true
					break
				}
			}
			_ = w.Close()
		}
		close(audio)
		<-done
		if trackOver || ctx.Err() != nil {
			return
		}
		if time.Since(began) > 30*time.Second {
			backoff = time.Second // it had been working; retry promptly
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// chanWriter hands Ogg pages to the transcriber; a stalled transcriber drops
// audio instead of blocking the RTP reader (bounded memory).
type chanWriter struct{ ch chan<- []byte }

func (c chanWriter) Write(p []byte) (int, error) {
	select {
	case c.ch <- append([]byte(nil), p...):
	default:
	}
	return len(p), nil
}

// playOgg writes an Ogg/Opus file's packets at real-time pace. first is
// called before the first packet goes out.
func playOgg(ctx context.Context, w sampleWriter, ogg []byte, first func()) error {
	return playOggReader(ctx, w, bytes.NewReader(ogg), first)
}

// playOggReader paces packets from an Ogg/Opus stream that may still be
// arriving (streamed TTS): the clock starts at the first packet, keeps a
// small lead so the receiver's jitter buffer never starves, and re-bases
// after a stall instead of bursting the backlog out.
func playOggReader(ctx context.Context, w sampleWriter, in io.Reader, first func()) error {
	r, _, err := oggreader.NewOggReader(in)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("speech is not Ogg/Opus: %w", err)
	}
	const lead = 60 * time.Millisecond
	var start time.Time
	var elapsed time.Duration
	for {
		pkt, err := r.ReadPacket()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		d, err := oggreader.ParsePacketDuration(pkt)
		if err != nil || d <= 0 {
			continue
		}
		if first != nil {
			first()
			first = nil
		}
		now := time.Now()
		if start.IsZero() || now.Sub(start.Add(elapsed)) > 200*time.Millisecond {
			start, elapsed = now, 0 // first packet, or the stream stalled: re-base
		}
		if err := w.WriteSample(media.Sample{Data: pkt, Duration: d}, nil); err != nil {
			return err
		}
		elapsed += d
		if wait := time.Until(start.Add(elapsed - lead)); wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}

// DeepgramTranscriber streams containerized Ogg/Opus to Deepgram's live API
// (nova-3, endpointing 300 ms, keyterm "Rysh" so the wake word is spelled as
// such more often) and emits each endpointed utterance. WebRTC
// senders go quiet in silence (DTX, or a finished file), so after 700 ms
// without audio it asks Deepgram to Finalize instead of waiting for more.
func DeepgramTranscriber(key string) Transcriber {
	return NewTranscriber(ProviderDeepgram, "", key, nil)
}

// deepgramKeytermEvery is how often a live Deepgram stream re-reads the
// agent names; a change is sent as a Configure (keyterms replaced
// mid-stream, nova-3 and Flux). Tests shorten it.
var deepgramKeytermEvery = 5 * time.Second

// deepgramTranscriber connects to endpoint(terms) with terms =
// sttKeyterms(names()) and keeps the stream's keyterms equal to it.
func deepgramTranscriber(key string, endpoint func(terms []string) string, names func() []string) Transcriber {
	return func(ctx context.Context, audio <-chan []byte, emit func(string)) error {
		terms := sttKeyterms(names())
		d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		conn, res, err := d.DialContext(ctx, endpoint(terms), http.Header{"Authorization": []string{"Token " + key}})
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		if err != nil {
			return errors.New("deepgram connect failed")
		}
		defer conn.Close()
		conn.SetReadLimit(1 << 20)
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			var acc []string
			for {
				var m struct {
					Type         string `json:"type"`
					IsFinal      bool   `json:"is_final"`
					SpeechFinal  bool   `json:"speech_final"`
					FromFinalize bool   `json:"from_finalize"`
					Channel      struct {
						Alternatives []struct {
							Transcript string `json:"transcript"`
						} `json:"alternatives"`
					} `json:"channel"`
				}
				if conn.ReadJSON(&m) != nil {
					return
				}
				if m.Type != "Results" || !m.IsFinal {
					continue
				}
				if len(m.Channel.Alternatives) > 0 && strings.TrimSpace(m.Channel.Alternatives[0].Transcript) != "" {
					acc = append(acc, strings.TrimSpace(m.Channel.Alternatives[0].Transcript))
				}
				if (m.SpeechFinal || m.FromFinalize) && len(acc) > 0 {
					emit(strings.Join(acc, " "))
					acc = nil
				}
			}
		}()
		var mu sync.Mutex
		send := func(kind int, p []byte) error {
			mu.Lock()
			defer mu.Unlock()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return conn.WriteMessage(kind, p)
		}
		idle := time.NewTimer(time.Hour)
		defer idle.Stop()
		keep := time.NewTicker(5 * time.Second)
		defer keep.Stop()
		reterm := time.NewTicker(deepgramKeytermEvery)
		defer reterm.Stop()
		pending := false
		for {
			select {
			case <-ctx.Done():
				_ = send(websocket.TextMessage, []byte(`{"type":"CloseStream"}`))
				return ctx.Err()
			case <-readDone:
				return errors.New("deepgram stream closed")
			case p, ok := <-audio:
				if !ok {
					_ = send(websocket.TextMessage, []byte(`{"type":"CloseStream"}`))
					select {
					case <-readDone:
					case <-time.After(3 * time.Second):
					}
					return nil
				}
				if send(websocket.BinaryMessage, p) != nil {
					return errors.New("deepgram send failed")
				}
				pending = true
				idle.Reset(700 * time.Millisecond)
			case <-idle.C:
				if pending {
					pending = false
					_ = send(websocket.TextMessage, []byte(`{"type":"Finalize"}`))
				}
			case <-keep.C:
				_ = send(websocket.TextMessage, []byte(`{"type":"KeepAlive"}`))
			case <-reterm.C:
				if now := sttKeyterms(names()); !slices.Equal(now, terms) {
					b, _ := json.Marshal(map[string]any{"type": "Configure", "keyterms": now})
					if send(websocket.TextMessage, b) == nil {
						terms = now
					}
				}
			}
		}
	}
}

// NewOpusSpeech is the Recall route's TTS asking for Ogg/Opus instead of MP3:
// OpenAI gpt-4o-mini-tts (coral), Deepgram Aura 2 or ElevenLabs Flash v2.5.
func NewOpusSpeech(provider, key string) OpusSpeech {
	voiced := NewOpusVoiceSpeech(provider, key)
	return func(ctx context.Context, text string) ([]byte, error) { return voiced(ctx, "", text) }
}

// NewOpusSpeechStream is NewOpusSpeech returning the audio as it arrives, so
// playback can start on the first Ogg page. Both providers stream the body.
func NewOpusSpeechStream(provider, key string) SynthesizeStream {
	voiced := NewOpusVoiceSpeechStream(provider, key)
	return func(ctx context.Context, text string) (io.ReadCloser, error) { return voiced(ctx, "", text) }
}

// NewOpusVoiceSpeech is NewOpusSpeech in a chosen voice (an agent's; "" is
// the provider default), so agent bots in a rysh room sound distinct (D9).
func NewOpusVoiceSpeech(provider, key string) func(ctx context.Context, voice, text string) ([]byte, error) {
	return opusSpeechWithClient(provider, key, newHTTPClient(), openAISpeechURL, deepgramOpusSpeechURL)
}

// NewOpusVoiceSpeechStream is NewOpusSpeechStream in a chosen voice.
func NewOpusVoiceSpeechStream(provider, key string) func(ctx context.Context, voice, text string) (io.ReadCloser, error) {
	return opusSpeechStreamWithClient(provider, key, streamingHTTPClient(), openAISpeechURL, deepgramOpusSpeechURL)
}

const (
	openAISpeechURL       = "https://api.openai.com/v1/audio/speech"
	deepgramOpusSpeechURL = "https://api.deepgram.com/v1/speak?model=aura-2-thalia-en&encoding=opus&container=ogg"
)

// streamingHTTPClient bounds the wait for response headers, not the body:
// a long reply streams for as long as the turn context allows. 10 s, not the
// whole-file client's 20: a provider that has not started answering by then
// (seen once from OpenAI in the LK-1 latency runs) is better replaced by the
// spoken failure phrase than by more silence.
func streamingHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 10 * time.Second
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// opusSpeechRequest builds the TTS request; voice is an agent's (validated by
// bindAgent) or "" for the provider default (coral / the URL's model).
func opusSpeechRequest(ctx context.Context, provider, key, voice, text, openaiURL, deepgramURL string) (*http.Request, error) {
	if len(text) > 6000 {
		return nil, errors.New("reply exceeds speech limit")
	}
	provider, lang := splitSpeechSpec(provider)
	if provider == ProviderElevenLabs {
		return elevenLabsSpeechRequest(ctx, key, lang, voice, text, "opus_48000_64", true)
	}
	openAIVoice := "coral"
	if openAIVoices[voice] {
		openAIVoice = voice
	}
	endpoint, auth := openaiURL, "Bearer "+key
	payload := map[string]any{"model": "gpt-4o-mini-tts", "voice": openAIVoice, "input": text, "response_format": "opus"}
	if provider == "deepgram" {
		endpoint, auth = deepgramURL, "Token "+key
		payload = map[string]any{"text": text}
		if deepgramVoice.MatchString(voice) {
			u, err := url.Parse(deepgramURL)
			if err != nil {
				return nil, err
			}
			q := u.Query()
			q.Set("model", voice)
			u.RawQuery = q.Encode()
			endpoint = u.String()
		}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func opusSpeechWithClient(provider, key string, client *http.Client, openaiURL, deepgramURL string) func(ctx context.Context, voice, text string) ([]byte, error) {
	return func(ctx context.Context, voice, text string) ([]byte, error) {
		req, err := opusSpeechRequest(ctx, provider, key, voice, text, openaiURL, deepgramURL)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, speechRequestError(ctx)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return nil, speechStatusError(res)
		}
		audio, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
		if err != nil || len(audio) > 8<<20 || !bytes.HasPrefix(audio, []byte("OggS")) {
			return nil, errors.New("invalid or oversized Ogg/Opus speech response")
		}
		return audio, nil
	}
}

func opusSpeechStreamWithClient(provider, key string, client *http.Client, openaiURL, deepgramURL string) func(ctx context.Context, voice, text string) (io.ReadCloser, error) {
	return func(ctx context.Context, voice, text string) (io.ReadCloser, error) {
		req, err := opusSpeechRequest(ctx, provider, key, voice, text, openaiURL, deepgramURL)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, speechRequestError(ctx)
		}
		if res.StatusCode != 200 {
			defer res.Body.Close()
			return nil, speechStatusError(res)
		}
		// Read the first bytes now so a wrong format fails before playback.
		br := bufio.NewReader(io.LimitReader(res.Body, 8<<20))
		if head, err := br.Peek(4); err != nil || string(head) != "OggS" {
			res.Body.Close()
			return nil, errors.New("invalid Ogg/Opus speech response")
		}
		return readCloser{br, res.Body}, nil
	}
}

type readCloser struct {
	io.Reader
	io.Closer
}
