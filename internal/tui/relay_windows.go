// SPDX-License-Identifier: Apache-2.0

//go:build windows

package tui

import (
	"errors"
	"io"

	"github.com/nats-io/nats.go"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ErrRelayEscape is returned by PTYRelay.Run when the user presses Ctrl+O.
var ErrRelayEscape = errors.New("relay: ctrl+o escape")

// ErrRelayLayout is returned by PTYRelay.Run when the user presses Ctrl+L.
var ErrRelayLayout = errors.New("relay: ctrl+l layout")

// ErrRelayModeSwitch is returned by PTYRelay.Run when the user presses Esc twice.
var ErrRelayModeSwitch = errors.New("relay: esc esc mode switch")

// PTYRelay is a no-op stub on Windows. Interactive PTY relay requires a Unix
// terminal (SIGWINCH, /dev/tty) and is not supported on Windows.
type PTYRelay struct{}

// NewPTYRelay returns a stub relay on Windows.
func NewPTYRelay(_ *nats.Conn, _ *msgpkg.NATSPublisher, _ string) *PTYRelay {
	return &PTYRelay{}
}

func (r *PTYRelay) SetStdin(io.Reader)  {}
func (r *PTYRelay) SetStdout(io.Writer) {}
func (r *PTYRelay) SetStderr(io.Writer) {}

// Run immediately returns an error on Windows since PTY relay is unsupported.
func (r *PTYRelay) Run() error {
	return errors.New("PTY relay is not supported on Windows")
}
