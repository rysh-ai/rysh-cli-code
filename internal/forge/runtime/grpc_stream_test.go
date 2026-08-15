// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

func watchOp() *ir.Operation {
	return &ir.Operation{
		ID:     "Feed_Watch",
		Method: "POST",
		Path:   "/feed.Feed/Watch",
		RequestBody: &ir.Schema{Type: "object", Properties: map[string]*ir.Schema{
			"topic": {Type: "string"},
		}},
	}
}

// waitFrames polls until the session has received at least n frames (or times
// out), returning everything read. Real-clock friendly: the fake server
// controls pacing, the test just drains.
func waitFrames(t *testing.T, m *StreamManager, id string, n int, wantDone bool) ([]string, *PollResult) {
	t.Helper()
	var all []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := m.Poll(id, 0)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		all = append(all, res.Frames...)
		if len(all) >= n && (!wantDone || res.Done) {
			return all, res
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frames (done=%v); got %v", n, wantDone, all)
	return nil, nil
}

// TestGRPCStreamNDJSON: start consumes grpc-gateway NDJSON {"result":…} frames
// (flushed with delays) into the session; polls are incremental; the stream
// end is reported.
func TestGRPCStreamNDJSON(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/json")
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, `{"result":{"text":"msg-%d"}}`+"\n", i)
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.StartServerStream(m, watchOp(), map[string]any{"topic": "news"})
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}

	frames, res := waitFrames(t, m, id, 3, true)
	if len(frames) != 3 || !strings.Contains(frames[0], "msg-1") || !strings.Contains(frames[2], "msg-3") {
		t.Fatalf("frames = %v, want the 3 result payloads in order", frames)
	}
	if !res.Done || res.Reason != "stream ended" {
		t.Fatalf("end state = %+v, want a clean 'stream ended'", res)
	}
	if gotPath != "/feed.Feed/Watch" {
		t.Errorf("request path = %q, want the gRPC wire path", gotPath)
	}
	if !strings.Contains(gotBody, `"topic":"news"`) {
		t.Errorf("request body = %q, want the JSON request message", gotBody)
	}
}

// TestGRPCStreamErrorFrame: a grpc-gateway {"error":…} frame ends the session
// with that error as the reason; frames before it stay pollable.
func TestGRPCStreamErrorFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `{"result":{"text":"ok-1"}}`)
		fl.Flush()
		fmt.Fprintln(w, `{"error":{"code":13,"message":"backend exploded"}}`)
		fl.Flush()
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 1, true)
	if !strings.Contains(frames[0], "ok-1") {
		t.Fatalf("frame before the error must be delivered, got %v", frames)
	}
	if !res.Done || !strings.Contains(res.Reason, "backend exploded") {
		t.Fatalf("end reason = %q, want the error frame's message", res.Reason)
	}
}

// TestGRPCStreamEarlyDisconnect: the server dying mid-stream ends the session
// with an error reason instead of hanging.
func TestGRPCStreamEarlyDisconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `{"result":{"text":"first"}}`)
		fl.Flush()
		// Kill the connection without a clean end-of-stream.
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, res := waitFrames(t, m, id, 1, true)
	if !strings.Contains(frames[0], "first") {
		t.Fatalf("frames = %v, want the delivered frame", frames)
	}
	if !res.Done || res.Reason == "" || res.Reason == "stream ended" {
		t.Fatalf("end state = %+v, want an abnormal-end reason", res)
	}
}

// TestGRPCStreamStopCancelsRequest: Stop cancels the in-flight HTTP request
// (the server observes its request context closing).
func TestGRPCStreamStopCancelsRequest(t *testing.T) {
	serverSawCancel := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `{"result":{"text":"hello"}}`)
		fl.Flush()
		<-r.Context().Done() // hold the stream open until the client cancels
		close(serverSawCancel)
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	waitFrames(t, m, id, 1, false)

	if _, err := m.Stop(id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-serverSawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("server never observed the request being cancelled after Stop")
	}
}

// TestGRPCStreamHTTPError: a non-2xx response fails the session with the
// status and body in the reason.
func TestGRPCStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"no such stream"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	_, res := waitFrames(t, m, id, 0, true)
	if !strings.Contains(res.Reason, "HTTP 404") {
		t.Fatalf("end reason = %q, want the HTTP status", res.Reason)
	}
}

// TestGRPCStreamRedaction: frames pass through the executor's redactor before
// entering the ring (governance parity with unary forge calls).
func TestGRPCStreamRedaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"result":{"token":"sekrit-value"}}`)
	}))
	defer srv.Close()

	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{
		Redact: func(s string) string { return strings.ReplaceAll(s, "sekrit-value", "[REDACTED]") },
	})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.StartServerStream(m, watchOp(), nil)
	if err != nil {
		t.Fatalf("StartServerStream: %v", err)
	}
	frames, _ := waitFrames(t, m, id, 1, true)
	if strings.Contains(frames[0], "sekrit-value") || !strings.Contains(frames[0], "[REDACTED]") {
		t.Fatalf("frame not redacted: %q", frames[0])
	}
}
