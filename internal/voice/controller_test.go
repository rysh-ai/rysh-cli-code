// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestControllerHappyPath(t *testing.T) {
	c := NewController(NewStaticRecorder([]byte("audio-bytes")), NewStaticTranscriber("hello there", nil))

	if c.State() != StateIdle {
		t.Fatalf("initial state = %v, want idle", c.State())
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c.State() != StateRecording {
		t.Fatalf("after Start state = %v, want recording", c.State())
	}

	path, err := c.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if c.State() != StateTranscribing {
		t.Fatalf("after Stop state = %v, want transcribing", c.State())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("audio file should exist after Stop: %v", err)
	}

	text, err := c.Transcribe(context.Background(), path)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "hello there" {
		t.Errorf("text = %q, want %q", text, "hello there")
	}
	if c.State() != StateIdle {
		t.Errorf("after Transcribe state = %v, want idle", c.State())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("audio file should be deleted after Transcribe, stat err = %v", err)
	}
}

func TestControllerStartTwiceFails(t *testing.T) {
	c := NewController(NewStaticRecorder([]byte("x")), NewStaticTranscriber("t", nil))
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Start(); err == nil {
		t.Error("second Start should fail while recording")
	}
}

func TestControllerCancel(t *testing.T) {
	c := NewController(NewStaticRecorder([]byte("x")), NewStaticTranscriber("t", nil))
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.Cancel()
	if c.State() != StateIdle {
		t.Errorf("after Cancel state = %v, want idle", c.State())
	}
	// After cancel we can start again.
	if err := c.Start(); err != nil {
		t.Errorf("Start after Cancel: %v", err)
	}
}

func TestControllerTranscribeErrorResetsState(t *testing.T) {
	c := NewController(NewStaticRecorder([]byte("x")), NewStaticTranscriber("", errors.New("boom")))
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	path, err := c.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := c.Transcribe(context.Background(), path); err == nil {
		t.Error("expected transcribe error")
	}
	if c.State() != StateIdle {
		t.Errorf("state after failed transcribe = %v, want idle", c.State())
	}
}

func TestControllerStopWithoutRecording(t *testing.T) {
	c := NewController(NewStaticRecorder([]byte("x")), NewStaticTranscriber("t", nil))
	if _, err := c.Stop(); err == nil {
		t.Error("Stop without recording should fail")
	}
}
