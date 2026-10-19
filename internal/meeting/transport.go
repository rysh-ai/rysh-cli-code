// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"io"
	"time"
)

// Transport is the meeting backbone a Service talks through (design 042,
// LK-2). Recall joins someone else's meeting (Meet, Zoom, Teams); LiveKit
// joins a room rysh hosts. Everything above it — wake detection, allowlist,
// hands-free turns, agents, stop phrases, audit log, progress wall — is the
// same Service code for both.
//
// Methods named ...Locked run with Service.mu held and must not block on the
// network; the others run without it.
type Transport interface {
	// Kind names the backbone: "recall" or "livekit".
	Kind() string
	// Join puts the bot into the meeting and returns its id. A *joinError
	// says whether a failure is definite (nothing was created) or uncertain.
	Join(ctx context.Context) (id string, err error)
	// Leave removes the bot. A *leaveError carries the operator notice.
	Leave(ctx context.Context) error
	// Status reports admission/end; an error is a status outage whose text
	// is the pause notice.
	Status(ctx context.Context) (TransportStatus, error)
	// MediaReadyLocked reports whether audio can be delivered to the meeting.
	MediaReadyLocked() bool
	// SpeakLocked starts playing one reply; completion is reported through
	// Service.playbackLocked(turnID, ok). An error means delivery never
	// started.
	SpeakLocked(turnID string, audio []byte) error
	// ClearLocked stops any playback in progress.
	ClearLocked()
}

// captureSettler is implemented by transports whose speech-to-text already
// marks the end of an utterance (LiveKit: Deepgram endpointing), so a capture
// needs little or no extra quiet time before it runs. Others wait
// defaultCaptureSettle for more finalized segments.
type captureSettler interface {
	CaptureSettle() time.Duration
}

// streamSpeaker is implemented by transports that can play a reply while it
// is still being synthesized. Completion is reported like SpeakLocked; the
// transport closes audio when playback ends or is cleared.
type streamSpeaker interface {
	SpeakStreamLocked(turnID string, audio io.ReadCloser) error
}

// TransportStatus is one status observation.
type TransportStatus struct {
	Unknown  bool   // nothing to report yet; leave state as it is
	Admitted bool   // the bot is in the call and hears participants
	Ended    bool   // the meeting ended or the bot was removed
	Why      string // with Ended: the provider's reason, when it gave one
	Waiting  string // while joining: the host action the bot waits for
	Lost     string // after joining: why hearing stopped
}

// joinError is a failed Join. Definite: nothing exists remotely, so the
// local bridge closes (create_failed). Otherwise the remote side may exist
// (create_uncertain) and must never be retried blindly.
type joinError struct {
	definite bool
	msg      string // Reply.Error
	notice   string // Reply.Notice for the uncertain case
}

func (e *joinError) Error() string { return e.msg }

// leaveError is a failed Leave; notice tells the operator what still runs.
type leaveError struct{ msg, notice string }

func (e *leaveError) Error() string { return e.msg }

// Segment is one finalized transcript segment from one participant, in the
// transport-neutral form acceptLocked consumes.
type Segment struct {
	Participant int     // stable per participant for this meeting
	Name        string  // display label; never authority by itself
	Identity    string  // authenticated identity (LiveKit token); "" on Recall
	Host        bool    // the platform/room says this participant is the host
	Start, End  float64 // seconds, relative to the meeting's own clock
	Words       []string
}

// tr returns the Service's transport. Services built without one (tests,
// older construction) are Recall services, as every Service was before LK-2.
func (s *Service) tr() Transport {
	if s.transport == nil {
		return recallTransport{s}
	}
	return s.transport
}

// mediaReadyLocked: can the bridge hear and speak right now.
func (s *Service) mediaReadyLocked() bool { return s.tr().MediaReadyLocked() }

// hearingLocked: can the bot take in the meeting. A reporter never speaks,
// so it does not need the output-media socket a Recall bot speaks through.
func (s *Service) hearingLocked() bool { return s.report != nil || s.mediaReadyLocked() }

// finishReport ends a reporter bot's note-taking and starts its final
// report; the first reason given is the one the report states.
func (s *Service) finishReport(reason string) {
	if s.report != nil {
		s.report.finish(reason)
	}
}

// label names the speaker in notices: the authenticated identity when the
// transport has one, else the display name.
func (u Segment) label() string {
	if u.Identity != "" {
		return u.Identity
	}
	return u.Name
}
