package voice

import (
	"context"
	"errors"
	"os"
	"sync"
)

// State is the voice controller's lifecycle state.
type State int

const (
	// StateIdle: not recording or transcribing.
	StateIdle State = iota
	// StateRecording: capturing microphone audio.
	StateRecording
	// StateTranscribing: recording finished, transcription in flight.
	StateTranscribing
)

func (s State) String() string {
	switch s {
	case StateRecording:
		return "recording"
	case StateTranscribing:
		return "transcribing"
	default:
		return "idle"
	}
}

// Controller wires a Recorder and a Transcriber into the simple state machine
// used by the TUI: Start -> Stop -> Transcribe (or Cancel).
//
// It is safe for concurrent use: the TUI event loop drives Start/Stop/Cancel/
// State while the (blocking) HTTP transcription runs on a separate goroutine.
type Controller struct {
	rec Recorder
	tr  Transcriber

	mu    sync.Mutex
	state State
}

// NewController builds a Controller from a Recorder and Transcriber.
func NewController(rec Recorder, tr Transcriber) *Controller {
	return &Controller{rec: rec, tr: tr}
}

// State returns the current lifecycle state.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// ProviderName returns the transcription provider name (for diagnostics).
func (c *Controller) ProviderName() string {
	if c.tr == nil {
		return ""
	}
	return c.tr.Name()
}

// Start begins recording. It is a no-op error if not idle.
func (c *Controller) Start() error {
	c.mu.Lock()
	if c.state != StateIdle {
		c.mu.Unlock()
		return errors.New("voice: not idle")
	}
	c.mu.Unlock()

	if err := c.rec.Start(context.Background()); err != nil {
		return err
	}
	c.mu.Lock()
	c.state = StateRecording
	c.mu.Unlock()
	return nil
}

// Stop finalizes the recording and transitions to StateTranscribing. It returns
// the path to the captured audio file, which the caller must pass to Transcribe
// (Transcribe deletes the file when done). On error the state is reset to idle.
func (c *Controller) Stop() (string, error) {
	c.mu.Lock()
	if c.state != StateRecording {
		c.mu.Unlock()
		return "", errors.New("voice: not recording")
	}
	c.mu.Unlock()

	path, err := c.rec.Stop()
	c.mu.Lock()
	if err != nil {
		c.state = StateIdle
	} else {
		c.state = StateTranscribing
	}
	c.mu.Unlock()
	return path, err
}

// Transcribe sends the audio file to the configured provider and returns the
// recognized text. The audio file is always removed before returning. The
// controller is reset to idle on completion regardless of outcome.
func (c *Controller) Transcribe(ctx context.Context, audioPath string) (string, error) {
	defer func() {
		if audioPath != "" {
			os.Remove(audioPath)
		}
		c.mu.Lock()
		c.state = StateIdle
		c.mu.Unlock()
	}()
	if c.tr == nil {
		return "", errors.New("voice: no transcriber configured")
	}
	return c.tr.Transcribe(ctx, audioPath)
}

// Cancel aborts an in-progress recording and resets to idle. It is safe to call
// in any state; during transcription it only resets the state flag (the HTTP
// request, if any, is left to finish and its result should be ignored by the
// caller).
func (c *Controller) Cancel() {
	c.rec.Cancel()
	c.mu.Lock()
	c.state = StateIdle
	c.mu.Unlock()
}
