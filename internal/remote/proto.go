// SPDX-License-Identifier: Apache-2.0

// Package remote carries a remote rysh session's panes to a local rysh over a
// byte stream — in production the stdin/stdout of `ssh <alias> rysh_local
// remote-serve` (design 070, feature #14).
//
// The wire is newline-delimited JSON frames. The client (the local rysh) opens
// with a hello carrying ProtoVersion; the server (remote-serve) answers with
// its own hello, or with an error frame and exit when the versions differ.
// After that the server streams layout and screen frames, and the client sends
// command frames, each of which the server acks. A command that is not acked is
// reported as not delivered: the caller shows an error and never re-routes the
// input to a local pane.
//
// The package knows nothing about NATS, actors or ssh keys: the server side
// reads a Backend (cmd/rysh wires one onto the remote daemon), the client side
// reads any io.ReadWriteCloser (in production an ssh subprocess).
package remote

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// ProtoVersion is the wire protocol both sides must agree on. Bump it on any
// frame change an older peer would misread.
const ProtoVersion = 1

// maxLine bounds one frame. A layout of many panes with 16 KB output tails
// stays well inside it.
const maxLine = 8 << 20

// Frame types.
const (
	FrameHello  = "hello"
	FrameError  = "error"
	FrameLayout = "layout"
	FrameScreen = "screen"
	FrameCmd    = "cmd"
	FrameAck    = "ack"
	FramePing   = "ping"
	FramePong   = "pong"
)

// Command types a client may send. They are the control-mode command types the
// mirror-tab path already produces (MsgUpstreamSendCommand), so a remote tab is
// driven by exactly the same TUI gestures as a control-mode shared tab.
const (
	CmdRawKeystroke = "raw_keystroke" // Payload: base64 bytes for the pane's PTY
	CmdExecShell    = "exec_shell"    // Payload: a shell line
	CmdSubmitInput  = "submit_input"  // alias of exec_shell
	CmdExecPrompt   = "exec_prompt"   // Payload: an AI prompt
	CmdExecChat     = "exec_chat"     // Payload: a chat message
	CmdExecRysh     = "exec_rysh"     // Payload: a ## command body, without "##"
	CmdTabOp        = "tab_op"        // Payload: JSON {op,dir,delta,name}
	CmdMaximize     = "maximize"      // Payload: JSON {on,rows,cols}
	CmdResize       = "resize"        // Payload: JSON {rows,cols}
)

// KnownCommand reports whether t is a command type the server will apply.
func KnownCommand(t string) bool {
	switch t {
	case CmdRawKeystroke, CmdExecShell, CmdSubmitInput, CmdExecPrompt, CmdExecChat,
		CmdExecRysh, CmdTabOp, CmdMaximize, CmdResize:
		return true
	}
	return false
}

// Error codes carried by an error frame.
const (
	CodeProtoMismatch = "proto_mismatch"
	CodeBadFrame      = "bad_frame"
	CodeBackend       = "backend"
)

var (
	// ErrProtoMismatch is returned by both sides when the peers' ProtoVersion
	// differ. Nothing else is exchanged.
	ErrProtoMismatch = errors.New("remote rysh protocol mismatch")
	// ErrNotDelivered is returned when a command was not acked in time or the
	// transport died first. The input must be treated as lost.
	ErrNotDelivered = errors.New("input not delivered to the remote pane")
	// ErrClosed is returned once the connection has ended.
	ErrClosed = errors.New("remote connection closed")
)

// Frame is one line on the wire. Only the fields of its type are set.
type Frame struct {
	T  string `json:"t"`
	ID uint64 `json:"id,omitempty"`

	// hello
	Proto   int    `json:"proto,omitempty"`
	Version string `json:"version,omitempty"`
	Session string `json:"session,omitempty"`

	// error
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	// layout
	Tabs []domain.TabSnapshot `json:"tabs,omitempty"`

	// screen
	Pane        string   `json:"pane,omitempty"`
	Interactive bool     `json:"interactive,omitempty"`
	Screen      []string `json:"screen,omitempty"`
	CursorRow   int      `json:"cursor_row,omitempty"`
	CursorCol   int      `json:"cursor_col,omitempty"`
	// Text is a non-interactive pane's output tail (Interactive false).
	Text string `json:"text,omitempty"`

	// cmd (Pane is the target)
	Cmd     string `json:"cmd,omitempty"`
	Payload string `json:"payload,omitempty"`

	// ack
	OK bool `json:"ok,omitempty"`
}

// Hello is what a peer announced in its hello frame.
type Hello struct {
	Proto   int
	Version string
	Session string
}

// Command is one input for a remote pane.
type Command struct {
	Type    string
	Pane    string
	Payload string
}

// Screen is a pane's current terminal screen.
// Interactive panes (an alternate-screen program, or any raw PTY app) carry
// their VT rows; the others carry Text, their output tail.
type Screen struct {
	Interactive bool
	Rows        []string
	CursorRow   int
	CursorCol   int
	Text        string
}

// mismatchError formats ErrProtoMismatch naming both versions.
func mismatchError(local, remote int, remoteVersion string) error {
	v := ""
	if remoteVersion != "" {
		v = " (" + remoteVersion + ")"
	}
	return fmt.Errorf("%w: remote speaks protocol %d%s, this rysh speaks %d — upgrade one side",
		ErrProtoMismatch, remote, v, local)
}

// frameWriter serialises frames onto w, one per line.
type frameWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newFrameWriter(w io.Writer) *frameWriter {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &frameWriter{enc: enc}
}

func (fw *frameWriter) write(f Frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.enc.Encode(f)
}

// frameReader reads frames, one per line.
type frameReader struct {
	sc *bufio.Scanner
}

func newFrameReader(r io.Reader) *frameReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	return &frameReader{sc: sc}
}

// next returns the next line, raw. io.EOF at a clean end.
func (fr *frameReader) next() ([]byte, error) {
	if !fr.sc.Scan() {
		if err := fr.sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return fr.sc.Bytes(), nil
}
