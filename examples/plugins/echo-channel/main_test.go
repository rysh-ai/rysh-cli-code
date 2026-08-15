// SPDX-License-Identifier: Apache-2.0

package main

// Drives run() in-process over pipes — the same newline-delimited JSON-RPC
// bytes rysh's stdio transport would send. Also the compile guard for the
// example: `go test ./examples/...` fails if the plugin stops building.

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// harness runs the plugin over pipes and decodes its output frames.
type harness struct {
	t      *testing.T
	stdin  io.WriteCloser
	frames <-chan frame
	done   <-chan error
}

func startPlugin(t *testing.T) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	frames := make(chan frame, 64)
	go func() {
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
		for scanner.Scan() {
			var f frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				t.Errorf("plugin emitted unparseable line %q: %v", scanner.Text(), err)
				continue
			}
			frames <- f
		}
		close(frames)
	}()

	done := make(chan error, 1)
	go func() {
		err := run("echo", inR, outW)
		outW.Close()
		done <- err
	}()

	t.Cleanup(func() { inW.Close() })
	return &harness{t: t, stdin: inW, frames: frames, done: done}
}

func (h *harness) send(v any) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.stdin.Write(append(b, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) request(id, method string, params any) {
	h.t.Helper()
	raw, _ := json.Marshal(params)
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(raw)})
}

// next returns the next frame matching pred within the timeout.
func (h *harness) next(what string, pred func(frame) bool) frame {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-h.frames:
			if !ok {
				h.t.Fatalf("plugin output closed while waiting for %s", what)
			}
			if pred(f) {
				return f
			}
		case <-deadline:
			h.t.Fatalf("timeout waiting for %s", what)
		}
	}
}

func idIs(f frame, id string) bool {
	var s string
	return json.Unmarshal(f.ID, &s) == nil && s == id
}

func TestEchoPluginProtocol(t *testing.T) {
	h := startPlugin(t)

	// ready must come unprompted, first.
	h.next("ready", func(f frame) bool { return f.Method == "ready" })

	// start → ok reply + connected status.
	h.request("1", "start", map[string]any{"enabled": true, "reply_mode": "mentions"})
	resp := h.next("start reply", func(f frame) bool { return idIs(f, "1") })
	if resp.Error != nil {
		t.Fatalf("start error: %+v", resp.Error)
	}
	st := h.next("status", func(f frame) bool { return f.Method == "status" })
	var status channelStatus
	if err := json.Unmarshal(st.Params, &status); err != nil || !status.Connected {
		t.Fatalf("status = %s (err %v), want connected", st.Params, err)
	}

	// send → ok reply + the echo inbound on the same thread.
	h.request("2", "send", outboundMessage{RecipientID: "r", Content: "hi there", ThreadID: "t-42"})
	if resp := h.next("send reply", func(f frame) bool { return idIs(f, "2") }); resp.Error != nil {
		t.Fatalf("send error: %+v", resp.Error)
	}
	in := h.next("echo inbound", func(f frame) bool { return f.Method == "inbound" })
	var im inboundMessage
	if err := json.Unmarshal(in.Params, &im); err != nil {
		t.Fatal(err)
	}
	if im.Content != "echo: hi there" || im.ThreadID != "t-42" {
		t.Fatalf("echo inbound = %+v", im)
	}

	// A Kind=="step" send is echoed with its kind visible.
	h.request("3", "send", outboundMessage{Content: "building", ThreadID: "t-42", Kind: "step"})
	h.next("step reply", func(f frame) bool { return idIs(f, "3") })
	in = h.next("step echo", func(f frame) bool { return f.Method == "inbound" })
	if err := json.Unmarshal(in.Params, &im); err != nil {
		t.Fatal(err)
	}
	if im.Content != "echo: [step] building" {
		t.Fatalf("step echo content = %q", im.Content)
	}

	// status request → reply carries reply_mode from start.
	h.request("4", "status", nil)
	resp = h.next("status reply", func(f frame) bool { return idIs(f, "4") })
	if err := json.Unmarshal(resp.Result, &status); err != nil || !status.Connected {
		t.Fatalf("status result = %s (err %v)", resp.Result, err)
	}

	// stop → ok reply, then run() returns nil.
	h.request("5", "stop", nil)
	h.next("stop reply", func(f frame) bool { return idIs(f, "5") })
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plugin did not exit after stop")
	}
}

func TestEchoPluginSendBeforeStartErrors(t *testing.T) {
	h := startPlugin(t)
	h.next("ready", func(f frame) bool { return f.Method == "ready" })
	h.request("1", "send", outboundMessage{Content: "too early"})
	resp := h.next("send reply", func(f frame) bool { return idIs(f, "1") })
	if resp.Error == nil {
		t.Fatal("send before start must return an error object")
	}
}
