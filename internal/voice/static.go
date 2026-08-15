// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"context"
	"os"
)

// staticRecorder is a test double that "records" by writing canned bytes to a
// temp file. It performs no real audio capture.
type staticRecorder struct {
	// payload is written to the temp file on Stop. If shorter than the
	// systemRecorder minimum it may be rejected by callers that size-check,
	// so tests should provide enough bytes when that matters.
	payload   []byte
	startErr  error
	stopErr   error
	recording bool
	path      string
}

// NewStaticRecorder returns a Recorder that writes payload to a temp file on
// Stop. Useful for tests without a microphone.
func NewStaticRecorder(payload []byte) Recorder {
	return &staticRecorder{payload: payload}
}

func (r *staticRecorder) Recording() bool { return r.recording }

func (r *staticRecorder) Start(ctx context.Context) error {
	if r.startErr != nil {
		return r.startErr
	}
	r.recording = true
	return nil
}

func (r *staticRecorder) Stop() (string, error) {
	r.recording = false
	if r.stopErr != nil {
		return "", r.stopErr
	}
	f, err := os.CreateTemp("", "rysh-voice-static-*.wav")
	if err != nil {
		return "", err
	}
	if len(r.payload) > 0 {
		_, _ = f.Write(r.payload)
	}
	_ = f.Close()
	r.path = f.Name()
	return r.path, nil
}

func (r *staticRecorder) Cancel() {
	r.recording = false
	if r.path != "" {
		os.Remove(r.path)
		r.path = ""
	}
}

// staticTranscriber is a test double that returns a canned transcript.
type staticTranscriber struct {
	text string
	err  error
}

// NewStaticTranscriber returns a Transcriber that always yields text (or err).
func NewStaticTranscriber(text string, err error) Transcriber {
	return &staticTranscriber{text: text, err: err}
}

func (t *staticTranscriber) Name() string { return "static" }

func (t *staticTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if t.err != nil {
		return "", t.err
	}
	return t.text, nil
}
