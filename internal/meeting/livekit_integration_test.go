// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveKitHostEndToEndWithFakes runs `host` for real against a LiveKit
// server — room created over the API, bot joined with its minted token,
// the participant's own track transcribed, the Service's wake/allowlist/
// hands-free turn, reply played on the bot's track — with fake STT,
// executor and TTS. Needs a LiveKit dev server, e.g.
//
//	docker run -d -p 7880:7880 -p 7881:7881 -p 7882:7882/udp livekit/livekit-server --dev --bind 0.0.0.0 --node-ip 127.0.0.1
//	LIVEKIT_TEST_URL=ws://127.0.0.1:7880 go test -run TestLiveKitHostEndToEnd ./internal/meeting
//
// On macOS use scripts/lk-live-tests.sh, which runs it off the host.
func TestLiveKitHostEndToEndWithFakes(t *testing.T) {
	url := os.Getenv("LIVEKIT_TEST_URL")
	if url == "" {
		t.Skip("set LIVEKIT_TEST_URL (dev server keys devkey/secret) to run")
	}
	// On macOS pion binds every interface (ICE host candidates, wildcard STUN
	// and mDNS sockets; server-sdk-go exposes no interface filter), which
	// raises the firewall's "accept incoming connections?" dialog for each
	// rebuilt test binary. scripts/lk-live-tests.sh runs this in a private
	// Docker network namespace instead, where 127.0.0.1 is the LiveKit server.
	if runtime.GOOS == "darwin" && os.Getenv("LK_LIVE_ALLOW_HOST") != "1" {
		t.Skip("run via scripts/lk-live-tests.sh (or set LK_LIVE_ALLOW_HOST=1 to accept the macOS firewall prompt)")
	}
	// Fake STT: once a participant's audio has flowed and gone quiet, "hear" it.
	var sawKey atomic.Value
	oldDial, oldSpeech, oldStream := DialLiveKit, LiveKitSpeech, LiveKitSpeechStream
	t.Cleanup(func() { DialLiveKit, LiveKitSpeech, LiveKitSpeechStream = oldDial, oldSpeech, oldStream })
	DialLiveKit = func(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error) {
		sawKey.Store(opts.STTKey)
		return connectLiveKit(ctx, serverURL, token, func(ctx context.Context, audio <-chan []byte, emit func(string)) error {
			n := 0
			for {
				select {
				case <-ctx.Done():
					return nil
				case p, ok := <-audio:
					if !ok {
						return nil
					}
					n += len(p)
				case <-time.After(700 * time.Millisecond):
					if n > 2000 {
						emit("hey rysh, what is two plus two?")
						n = 0
					}
				}
			}
		})
	}
	LiveKitSpeech = func(string, string) Synthesize {
		return func(context.Context, string) ([]byte, error) { return fakeOgg(t, 50), nil }
	}
	// Replies stream (PlayStream over WebRTC), as with the real TTS.
	LiveKitSpeechStream = func(string, string) SynthesizeStream {
		return func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(fakeOgg(t, 50))), nil
		}
	}
	var asked atomic.Value
	execute := func(_ context.Context, req string) (string, error) { asked.Store(req); return "Four.", nil }
	room := fmt.Sprintf("lk-test-%d", time.Now().UnixNano())
	mgr := NewManager()
	t.Cleanup(mgr.Close)
	c := Command{Action: "host", Room: room, TTS: "openai", Minutes: 5}
	c.Defaults()
	r := mgr.Do(c, Credentials{Speech: "tts-key", LiveKit: LiveKitCredentials{URL: url, APIKey: "devkey", APISecret: "secret", STTKey: "stt-key"}}, execute)
	if r.Error != "" || r.JoinURL == "" {
		t.Fatal(r)
	}
	if sawKey.Load() != "stt-key" {
		t.Fatal("STT key did not reach the room client")
	}
	waitPhase := func(want string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if mgr.Do(Command{Action: "status"}, Credentials{}, nil).Phase == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("phase never reached %s: %+v", want, mgr.Do(Command{Action: "status"}, Credentials{}, nil))
	}
	waitPhase("listening") // admitted once connected (first status poll)
	probe, err := JoinLiveKitProbe(LiveKitConfig{URL: url, APIKey: "devkey", APISecret: "secret", Room: room}, "host")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := probe.Say(ctx, fakeOgg(t, 100)); err != nil {
		t.Fatal(err)
	}
	waitPhase("speaking")
	if asked.Load() != "what is two plus two?" {
		t.Fatalf("executor asked %v", asked.Load())
	}
	spokeAt := time.Now()
	waitPhase("listening") // played → back to listening
	n, first, _, from, _ := probe.Heard()
	if n < 40 || from != liveKitBotIdentity {
		t.Fatalf("participant heard %d audible packets from %q, want ≥40 from the bot", n, from)
	}
	if first.After(spokeAt.Add(2 * time.Second)) {
		t.Fatal("reply audio arrived long after the bot said it was speaking")
	}
}
