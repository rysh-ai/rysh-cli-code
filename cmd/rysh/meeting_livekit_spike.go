// SPDX-License-Identifier: Apache-2.0

package main

// rysh meeting spike-livekit — E29 LK-1 dev harness. Runs the LiveKit bot in
// this process (not the daemon) with the real executor, Deepgram STT and TTS.
// --synthetic adds a scripted participant that asks a question out loud and
// checks the bot answered with audio, then prints per-stage latency.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/livekit/protocol/auth"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/meetingexec"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

const spikeLiveKitUsage = `rysh meeting spike-livekit --room NAME [--url ws://127.0.0.1:7880] [--allow id,id|anyone]
       [--tts deepgram|elevenlabs|openai] [--join-page https://meet.livekit.io] [--synthetic "hey rysh, what is two plus two"]
       [--minutes 10] [--session NAME]
  rysh meeting spike-livekit --room NAME --token IDENTITY   (loopback --dev server only)

Dev harness (E29 LK-1): runs the same path as "rysh meeting host" in this
process instead of the daemon — room created, bot joined, each participant's
own track transcribed (Deepgram live), wake phrase + allowlist + hands-free
turn through the session executor (no tools), reply spoken on the bot's track.
Keys by name from the environment or the session secret files: LIVEKIT_API_KEY
/SECRET (devkey/secret for a loopback --dev server), DEEPGRAM_API_KEY,
OPENAI_API_KEY. --synthetic joins a scripted participant (identity "host")
that says the phrase, waits for the spoken answer, transcribes it and prints
latency; exits non-zero if no answer. --token prints a 1-hour dev join token
and link for another browser participant, then exits.
`

func runMeetingSpikeLiveKit(cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(spikeLiveKitUsage)
		return nil
	}
	rest, room := extractStringFlag(args, "--room")
	rest, lkURL := extractStringFlag(rest, "--url")
	rest, allow := extractStringFlag(rest, "--allow")
	rest, tts := extractStringFlag(rest, "--tts")
	rest, page := extractStringFlag(rest, "--join-page")
	rest, synthetic := extractStringFlag(rest, "--synthetic")
	rest, mins := extractStringFlag(rest, "--minutes")
	rest, sess := extractStringFlag(rest, "--session")
	rest, tokenFor := extractStringFlag(rest, "--token")
	if len(rest) != 0 || room == "" {
		return errors.New(spikeLiveKitUsage)
	}
	if lkURL == "" {
		lkURL = firstNonEmpty(os.Getenv("LIVEKIT_URL"), "ws://127.0.0.1:7880")
	}
	minutes := 10
	if mins != "" {
		if _, err := fmt.Sscanf(mins, "%d", &minutes); err != nil || minutes < 1 || minutes > 180 {
			return errors.New("--minutes must be 1–180")
		}
	}
	sess = firstNonEmpty(sess, os.Getenv("RYSH_SESSION"), cfg.SessionName)
	secret := func(name string) string { return spikeSecret(sess, name) }
	lk := meeting.LiveKitConfig{URL: lkURL, APIKey: secret("LIVEKIT_API_KEY"), APISecret: secret("LIVEKIT_API_SECRET"), Room: room}
	if lk.APIKey == "" && isLoopbackURL(lkURL) {
		lk.APIKey, lk.APISecret = "devkey", "secret" // livekit-server --dev
	}
	if lk.APIKey == "" || lk.APISecret == "" {
		return errors.New("set LIVEKIT_API_KEY and LIVEKIT_API_SECRET (env or session secrets)")
	}
	if tokenFor != "" {
		if !isLoopbackURL(lkURL) || lk.APIKey != "devkey" {
			return errors.New("--token is for a loopback livekit-server --dev only; rysh-server mints real tokens (LK-3)")
		}
		jwt, err := auth.NewAccessToken(lk.APIKey, lk.APISecret).SetIdentity(tokenFor).SetName(tokenFor).SetValidFor(time.Hour).
			SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: room}).ToJWT()
		if err != nil {
			return err
		}
		fmt.Println(jwt)
		fmt.Printf("%s/custom?liveKitUrl=%s&token=%s\n", firstNonEmpty(page, "https://meet.livekit.io"), url.QueryEscape(lkURL), jwt)
		return nil
	}

	c := meeting.Command{Action: "host", Room: room, TTS: tts, STT: meeting.ProviderDeepgram, Minutes: minutes}
	if allow != "" {
		for _, a := range strings.Split(allow, ",") {
			c.Allow = append(c.Allow, strings.TrimSpace(a))
		}
	}
	c.Defaults()
	dgKey := secret("DEEPGRAM_API_KEY")
	speechKey := secret(meeting.SpeechKeyName(c.TTS))
	if dgKey == "" || speechKey == "" {
		return errors.New("set DEEPGRAM_API_KEY (STT) and the --tts provider's key (" + meeting.SpeechKeyName(c.TTS) + ")")
	}
	execute, err := meetingexec.New(actor.NewActorSystem(), provider.NewAgenticProvider(cfg), "")
	if err != nil {
		return err
	}
	keys := meeting.Credentials{Speech: speechKey, LiveKit: meeting.LiveKitCredentials{URL: lkURL, APIKey: lk.APIKey, APISecret: lk.APISecret, JoinPage: firstNonEmpty(page, "https://meet.livekit.io"), STTKey: dgKey}}
	mgr := meeting.NewManager()
	defer mgr.Close()
	stamp := func(t time.Time) string { return t.Format("15:04:05.000") }
	r := mgr.Do(c, keys, execute)
	if r.Error != "" {
		return fmt.Errorf("host: %s (state %s)", r.Error, r.State)
	}
	fmt.Printf("%s room %q hosted at %s; bot joining (allow: %s)\njoin link (host identity): %s\n", stamp(time.Now()), r.Room, lkURL, strings.Join(c.Allow, ","), r.JoinURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(minutes)*time.Minute)
	defer cancel()
	phases := make(chan spikePhase, 64)
	go watchMeetingPhases(ctx, mgr, phases)
	if synthetic == "" {
		for p := range phases {
			fmt.Printf("%s phase %s %s\n", stamp(p.at), p.phase, p.detail)
		}
		return nil
	}
	return spikeSynthetic(ctx, lk, synthetic, meeting.NewOpusSpeech(c.TTS, speechKey), dgKey, phases)
}

type spikePhase struct {
	at            time.Time
	phase, detail string
}

// watchMeetingPhases polls status every 50 ms and reports phase changes, with
// the turn's request/reply once they exist (status wall vocabulary).
func watchMeetingPhases(ctx context.Context, mgr *meeting.Manager, out chan<- spikePhase) {
	defer close(out)
	last := ""
	for ctx.Err() == nil {
		r := mgr.Do(meeting.Command{Action: "pending"}, meeting.Credentials{}, nil)
		if r.Phase != last && r.Phase != "" {
			detail := ""
			if r.Request != "" {
				detail = fmt.Sprintf("request=%q", r.Request)
			}
			if r.Result != "" {
				detail += fmt.Sprintf(" reply=%q", r.Result)
			}
			if r.Notice != "" {
				detail += " (" + r.Notice + ")"
			}
			out <- spikePhase{time.Now(), r.Phase, detail}
			last = r.Phase
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// spikeSynthetic is the no-human end-to-end check.
func spikeSynthetic(ctx context.Context, lk meeting.LiveKitConfig, phrase string, speech meeting.OpusSpeech, dgKey string, phases <-chan spikePhase) error {
	question, err := speech(ctx, phrase)
	if err != nil {
		return fmt.Errorf("synthesize the question: %w", err)
	}
	stamp := func(t time.Time) string { return t.Format("15:04:05.000") }
	seen := map[string]time.Time{}
	var reply string
	waitPhase := func(want string, timeout time.Duration) bool {
		deadline := time.After(timeout)
		for {
			if _, ok := seen[want]; ok {
				return true
			}
			select {
			case p, ok := <-phases:
				if !ok {
					return false
				}
				fmt.Printf("%s phase %s %s\n", stamp(p.at), p.phase, p.detail)
				if _, dup := seen[p.phase]; !dup || p.phase == "listening" {
					seen[p.phase] = p.at
				}
				if strings.Contains(p.detail, "reply=") {
					reply = p.detail
				}
			case <-deadline:
				return false
			}
		}
	}
	if !waitPhase("listening", 30*time.Second) {
		return errors.New("FAIL: the bot never reached listening (did it join the room?)")
	}
	probe, err := meeting.JoinLiveKitProbe(lk, "host")
	if err != nil {
		return err
	}
	defer probe.Close()
	time.Sleep(1500 * time.Millisecond) // let both subscriptions settle
	delete(seen, "listening")
	fmt.Printf("synthetic participant %q says %q\n", "host", phrase)
	saidEnd, err := probe.Say(ctx, question)
	if err != nil {
		return err
	}
	if !waitPhase("speaking", 2*time.Minute) {
		return errors.New("FAIL: the bot did not start speaking within 2 minutes")
	}
	waitPhase("listening", time.Minute)
	for i := 0; i < 50; i++ { // let the last packets land
		if n, _, last, _, _ := probe.Heard(); n > 0 && time.Since(last) > time.Second {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	n, first, last, from, ogg := probe.Heard()
	if n == 0 {
		return errors.New("FAIL: no audio from the bot reached the synthetic participant")
	}
	heard, err := deepgramPrerecorded(ctx, dgKey, ogg)
	if err != nil {
		heard = "(transcription failed: " + err.Error() + ")"
	}
	ms := func(a, b time.Time) string {
		if a.IsZero() || b.IsZero() {
			return "-"
		}
		return fmt.Sprintf("%d ms", b.Sub(a).Milliseconds())
	}
	fmt.Printf("\nbot audio: %d audible packets from %q, %.1f s\n", n, from, last.Sub(first).Seconds())
	fmt.Printf("turn (bot side):          %s\n", reply)
	fmt.Printf("reply (STT of the audio): %q\n\n", heard)
	fmt.Println("latency (phases polled every 50 ms; heard is skipped when the request runs on the final utterance)")
	fmt.Printf("  end of question → thinking (STT final + settle)     %s\n", ms(saidEnd, seen["thinking"]))
	fmt.Printf("  thinking → preparing_speech (executor)              %s\n", ms(seen["thinking"], seen["preparing_speech"]))
	fmt.Printf("  preparing_speech → speaking (TTS stream opened)     %s\n", ms(seen["preparing_speech"], seen["speaking"]))
	fmt.Printf("  speaking → first audio at participant (1st audio page)%s\n", ms(seen["speaking"], first))
	fmt.Printf("  END OF QUESTION → FIRST REPLY AUDIO AT PARTICIPANT  %s\n", ms(saidEnd, first))
	if from != "rysh-bot" {
		return fmt.Errorf("FAIL: audio came from %q, not the bot", from)
	}
	fmt.Println("\nPASS: the bot heard the synthetic participant and answered with audio in the room")
	return nil
}

// deepgramPrerecorded transcribes the reply the tester received, as a check
// that the published audio is the answer and intelligible.
func deepgramPrerecorded(ctx context.Context, key string, ogg []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepgram.com/v1/listen?model=nova-3&smart_format=true", bytes.NewReader(ogg))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Token "+key)
	req.Header.Set("Content-Type", "audio/ogg")
	res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", errors.New("deepgram request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("deepgram returned HTTP %d", res.StatusCode)
	}
	var r struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&r); err != nil || len(r.Results.Channels) == 0 || len(r.Results.Channels[0].Alternatives) == 0 {
		return "", errors.New("unexpected deepgram response")
	}
	return r.Results.Channels[0].Alternatives[0].Transcript, nil
}

// spikeSecret reads a key by name: environment first, then the session's
// secret file (<workspace>/.rysh/secrets/<session>/<NAME>) found upward from
// the working directory. Values are never printed.
func spikeSecret(session, name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	if session == "" {
		return ""
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, ".rysh", "secrets", session, name)); err == nil {
			return strings.TrimSpace(string(b))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
