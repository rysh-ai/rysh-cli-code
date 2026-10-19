// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"github.com/rysh-ai/rysh-cli-code/internal/facetime"
)

// Runs on the attached user's Mac, never through the workspace/daemon bus.
type faceTimePicker struct {
	in  io.Reader
	out io.Writer
}

func (p *faceTimePicker) SetStdin(r io.Reader)  { p.in = r }
func (p *faceTimePicker) SetStdout(w io.Writer) { p.out = w }
func (p *faceTimePicker) SetStderr(w io.Writer) {}
func (p *faceTimePicker) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := tea.NewProgram(facetime.NewPicker(ctx, facetime.NativeClient{}), tea.WithInput(p.in), tea.WithOutput(p.out), tea.WithAltScreen()).Run()
	return err
}

type faceTimeClosed struct{ err error }
