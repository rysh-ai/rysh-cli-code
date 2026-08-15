// SPDX-License-Identifier: Apache-2.0

package web

// The clipboard's missing direction (E16 T3).
//
// Paste already worked, incidentally: a remote client intercepts a native paste
// event and forwards the bytes to the PTY as `raw_key_input`
// (rysh-cli-app/src/components/PaneBox.tsx). So a phone or a browser could
// always type INTO a pane. Nothing ever carried content the other way, which is
// the direction you actually want when the useful thing — a stack trace, a
// generated key, a command's output — is on a pane's screen and your terminal
// is in another building.
//
// clipboard_copy is a request/reply exchange over the existing socket, modelled
// on completion_get (W7) and for the same reason: the answer goes to the ASKING
// client only. A pane buffer carries whatever that pane has printed, tokens and
// private output included, and a broadcast would hand one viewer's copy to
// every other connected client.
//
// What this deliberately does NOT do is touch the system clipboard of the
// machine the server runs on. Writing the host's clipboard is the AI-facing
// clipboard tool's job (internal/tools/clipboard.go) and is a different surface
// with a different trust story. Here the server only hands text to a client,
// which then decides whether to write its own clipboard — the only clipboard a
// browser or a phone can write anyway, and only from a user gesture.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// clipboardMaxBytes bounds one clipboard_content reply.
//
// Same constraint as seedBatchMaxBytes, and chosen to match it: gorilla's write
// deadline (writeWait, 10s) applies to the whole write, and over a free ngrok
// tunnel measured at 95 KB/s a 256 KB message takes ~2.7s. A pane's buffer is
// not bounded by anything of the sort — one was measured carrying 682 KB — so
// without a ceiling a single clipboard_copy of a busy pane could blow the
// deadline and take the whole connection down with it, which is exactly the
// failure that made the seed a sequence of batches.
const clipboardMaxBytes = 256 * 1024

// clipboardCopyCmd is the clipboard_copy ws command payload.
type clipboardCopyCmd struct {
	RequestID string `json:"request_id"`
	PaneID    string `json:"pane_id"`
	// Source names the pane buffer to copy. Empty means "output", the shell
	// plane, which is what a user pointing at a pane almost always means.
	Source string `json:"source"`
	// MaxBytes may ask for LESS than clipboardMaxBytes; it cannot raise the
	// ceiling. 0 means "as much as you will give me".
	MaxBytes int `json:"max_bytes"`
}

// clipboardSources are the buffers a client may ask for, mapped to the pane
// snapshot fields that hold them. Keeping this a closed set (rather than
// reflecting over the snapshot) is deliberate: the snapshot carries provider
// names, upstream URLs and pane metadata too, and none of that should become
// readable just because it happens to live on the same struct.
var clipboardSources = []string{
	"output", "ai_output", "rysh_output", "chat_output", "external_output",
	"vt_screen", "remote_vt_screen",
}

// handleClipboardCopy answers one clipboard_copy, to the requesting client only.
//
// It runs off the readPump goroutine because it makes a bus round-trip: a slow
// or dead pane must not stall the client's inbound command stream.
func (s *Server) handleClipboardCopy(c *wsClient, data json.RawMessage) {
	var cmd clipboardCopyCmd
	if json.Unmarshal(data, &cmd) != nil || cmd.RequestID == "" || cmd.PaneID == "" {
		// No request_id means no way to answer, and no pane means nothing to
		// answer about. Both are client bugs and both are unanswerable, which
		// is the one case where this command is as silent as the rest of the
		// protocol.
		return
	}

	go func() {
		text, err := s.paneClipboardText(cmd)
		limit := clipboardMaxBytes
		if cmd.MaxBytes > 0 && cmd.MaxBytes < limit {
			limit = cmd.MaxBytes
		}
		capped := capTail(text, limit)

		s.replyClipboardContent(c, cmd, capped, len(capped) < len(text), err)
	}()
}

// paneClipboardText fetches one pane buffer over the bus.
func (s *Server) paneClipboardText(cmd clipboardCopyCmd) (string, string) {
	source := strings.TrimSpace(cmd.Source)
	if source == "" {
		source = "output"
	}
	if !containsString(clipboardSources, source) {
		return "", fmt.Sprintf("unknown clipboard source %q — expected one of %s",
			source, strings.Join(clipboardSources, ", "))
	}

	subject := msg.T("pane", cmd.PaneID, "inbox")
	reply, err := s.pub.Request(subject, &msg.MsgGetPaneSnapshot{}, time.Second)
	if err != nil {
		// Fail visible, like webpane_error: a copy that silently yields nothing
		// is indistinguishable from a pane that is genuinely empty, and the
		// user is left pressing the button again.
		return "", "pane did not answer: " + err.Error()
	}
	pr, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return "", "pane returned an unexpected reply"
	}
	p := pr.Snapshot

	switch source {
	case "output":
		return p.Output, ""
	case "ai_output":
		return p.AIOutput, ""
	case "rysh_output":
		return p.RyshOutput, ""
	case "chat_output":
		return p.ChatOutput, ""
	case "external_output":
		return p.ExternalOutput, ""
	case "vt_screen":
		return strings.Join(p.VTScreen, "\n"), ""
	case "remote_vt_screen":
		return strings.Join(p.RemoteVTScreen, "\n"), ""
	}
	return "", "unhandled clipboard source " + source
}

// replyClipboardContent sends the answer to one client. Never broadcast.
func (s *Server) replyClipboardContent(c *wsClient, cmd clipboardCopyCmd, text string, truncated bool, errMsg string) {
	source := strings.TrimSpace(cmd.Source)
	if source == "" {
		source = "output"
	}
	reply, err := json.Marshal(map[string]interface{}{
		"type": "clipboard_content",
		"data": map[string]interface{}{
			"request_id": cmd.RequestID,
			"pane_id":    cmd.PaneID,
			"source":     source,
			"text":       text,
			"truncated":  truncated,
			"err":        errMsg,
		},
	})
	if err != nil {
		return
	}
	select {
	case c.send <- reply:
	default: // client too slow or gone — drop, exactly as completion_result does
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
