// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A LiveKit utterance is already endpointed by Deepgram: the turn runs at
// once, not after Recall's 1.2 s settle or the next 250 ms tick.
func TestHostLiveKitRunsOnTheFinalUtteranceWithoutSettle(t *testing.T) {
	h := newHostRig(t)
	h.host(t, Command{})
	start := time.Now()
	h.say("host", "Host", "hey rysh what is the answer")
	if got := <-h.ran; got != "what is the answer" {
		t.Fatal(got)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("executor started %v after the utterance; want no settle", el)
	}

	// Wake phrase alone, then the request as the next utterance.
	h2 := newHostRig(t)
	h2.host(t, Command{})
	h2.say("host", "Host", "Hey, Rysh.")
	time.Sleep(50 * time.Millisecond)
	if st := h2.m.active().status(false); st.State != "capturing" {
		t.Fatalf("bare wake must wait for the request: %+v", st)
	}
	h2.say("host", "Host", "what is the answer")
	select {
	case got := <-h2.ran:
		if got != "what is the answer" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("request after a bare wake did not run")
	}
}

func TestRecallCaptureSettleUnchanged(t *testing.T) {
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.captureSettleLocked(); d != 1200*time.Millisecond {
		t.Fatalf("Recall settle %v, want 1.2 s", d)
	}
}

// streamRig makes the host Service stream replies from a pipe.
func streamRig(t *testing.T) (*hostRig, *io.PipeWriter, chan struct{}) {
	h := newHostRig(t)
	h.host(t, Command{})
	pr, pw := io.Pipe()
	opened := make(chan struct{}, 1)
	s := h.m.active()
	s.mu.Lock()
	s.speechStream = func(context.Context, string) (io.ReadCloser, error) { opened <- struct{}{}; return pr, nil }
	s.mu.Unlock()
	return h, pw, opened
}

func TestHostStreamedReplyPlaysAndStopAbandonsTheStream(t *testing.T) {
	h, pw, opened := streamRig(t)
	h.say("host", "Host", "hey rysh tell a long story")
	<-h.ran
	<-opened
	waitFor(t, func() bool { return h.m.active().status(false).State == "speaking" })
	// Speaking starts before the stream is complete; now someone says stop.
	h.say("guest", "Guest", "rysh stop")
	waitFor(t, func() bool { return h.m.active().status(false).State == "listening" })
	if _, err := pw.Write([]byte("more audio")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("stop must close the synthesis stream, write err %v", err)
	}
	if h.room.plays() != 0 {
		t.Fatal("stopped reply still played")
	}

	h2, pw2, opened2 := streamRig(t)
	h2.say("host", "Host", "hey rysh short answer")
	<-h2.ran
	<-opened2
	go func() { _, _ = pw2.Write([]byte("OGGDATA")); _ = pw2.Close() }()
	waitFor(t, func() bool { return h2.room.plays() == 1 && h2.m.active().status(false).State == "listening" })
	if string(h2.room.played[0]) != "OGGDATA" {
		t.Fatalf("played %q", h2.room.played[0])
	}
}

func TestPlayOggReaderStartsBeforeTheStreamEndsAndStopsOnCancel(t *testing.T) {
	ogg := fakeOgg(t, 60)
	pr, pw := io.Pipe()
	sink := &fakeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- playOggReader(ctx, sink, pr, nil) }()
	half := len(ogg) / 2
	if _, err := pw.Write(ogg[:half]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sink.count() > 0 })
	cancel()
	_ = pr.CloseWithError(context.Canceled) // what PlayStream's AfterFunc does
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop a stream waiting for data")
	}
	if sink.count() >= 60 {
		t.Fatal("played the whole reply despite cancel")
	}
}

func TestOpusSpeechStreamReturnsOnFirstBytes(t *testing.T) {
	ogg := fakeOgg(t, 20)
	var slow atomic.Bool
	slow.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			_, _ = w.Write(ogg[:64])
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
			_, _ = w.Write(ogg[64:])
			return
		}
		_, _ = w.Write([]byte("ID3 mp3"))
	}))
	defer srv.Close()
	stream := opusSpeechStreamWithClient("openai", "k", srv.Client(), srv.URL, srv.URL)
	start := time.Now()
	body, err := stream(context.Background(), "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("stream returned after %v; want the first page, not the whole body", el)
	}
	all, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(all, ogg) {
		t.Fatal("stream body differs")
	}
	slow.Store(false)
	if _, err := stream(context.Background(), "", "hello"); err == nil {
		t.Fatal("accepted a non-Ogg stream")
	}
}

// ClearLocked must abandon the synthesis stream before it returns, not when a
// context callback happens to run: the Service sets "listening" right after,
// and that state means the old reply is gone. Checked with no waiting.
func TestClearClosesTheSynthesisStreamSynchronously(t *testing.T) {
	for i := 0; i < 200; i++ {
		h, pw, opened := streamRig(t)
		h.say("host", "Host", "hey rysh tell a long story")
		<-h.ran
		<-opened
		waitFor(t, func() bool { return h.m.active().status(false).State == "speaking" })
		s := h.m.active()
		s.mu.Lock()
		s.tr().ClearLocked()
		s.mu.Unlock()
		if _, err := pw.Write([]byte("more audio")); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("run %d: stream still open right after ClearLocked (write err %v)", i, err)
		}
	}
}
