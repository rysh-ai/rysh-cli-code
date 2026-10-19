// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// LiveKitProbe is a synthetic human for the spike's end-to-end check: a
// STANDARD participant that speaks prepared Ogg/Opus into the room and records
// what the other participants (the bot) send back.
type LiveKitProbe struct {
	room  *lksdk.Room
	track *lksdk.LocalTrack

	mu      sync.Mutex
	packets int // audible packets since the last Say (see audible)
	first   time.Time
	last    time.Time
	from    string
	ogg     bytes.Buffer
}

// audible separates speech from the Opus silence frames (TOC 0xF8, then
// 0xFF 0xFE, zero-padded to 80 bytes) that arrive while the bot's track is
// idle (measured against livekit-server v1.13.7), and from DTX stubs.
func audible(payload []byte) bool {
	return len(payload) > 3 && !bytes.HasPrefix(payload, []byte{0xF8, 0xFF, 0xFE})
}

// JoinLiveKitProbe joins cfg.Room as identity (a normal participant).
func JoinLiveKitProbe(cfg LiveKitConfig, identity string) (*LiveKitProbe, error) {
	p := &LiveKitProbe{}
	cb := lksdk.NewRoomCallback()
	cb.ParticipantCallback.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go p.record(track, rp.Identity())
	}
	room, err := lksdk.ConnectToRoom(cfg.URL, lksdk.ConnectInfo{APIKey: cfg.APIKey, APISecret: cfg.APISecret, RoomName: cfg.Room, ParticipantIdentity: identity, ParticipantName: identity}, cb)
	if err != nil {
		return nil, fmt.Errorf("probe join: %w", err)
	}
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err == nil {
		_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "probe-mic", Source: livekit.TrackSource_MICROPHONE})
	}
	if err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("probe publish: %w", err)
	}
	p.room, p.track = room, track
	return p, nil
}

func (p *LiveKitProbe) record(track *webrtc.TrackRemote, identity string) {
	p.mu.Lock()
	w, err := oggwriter.NewWith(&p.ogg, 48000, 1)
	p.mu.Unlock()
	if err != nil {
		return
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			break
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		p.mu.Lock()
		if audible(pkt.Payload) {
			if p.packets == 0 {
				p.first, p.from = time.Now(), identity
			}
			p.packets++
			p.last = time.Now()
		}
		_ = w.WriteRTP(pkt)
		p.mu.Unlock()
	}
	p.mu.Lock()
	_ = w.Close()
	p.mu.Unlock()
}

// Say plays Ogg/Opus into the room in real time and returns when the last
// packet was sent.
func (p *LiveKitProbe) Say(ctx context.Context, ogg []byte) (time.Time, error) {
	p.mu.Lock()
	p.packets, p.first, p.last, p.from = 0, time.Time{}, time.Time{}, ""
	p.mu.Unlock()
	err := playOgg(ctx, p.track, ogg, nil)
	return time.Now(), err
}

// Heard reports audible audio received from other participants since the last
// Say: packet count, first and last packet time, sender identity, and all
// received audio as Ogg/Opus.
func (p *LiveKitProbe) Heard() (packets int, first, last time.Time, from string, ogg []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.packets, p.first, p.last, p.from, append([]byte(nil), p.ogg.Bytes()...)
}

// Close leaves the room.
func (p *LiveKitProbe) Close() { p.room.Disconnect() }
