// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rysh-ai/rysh-cli-code/internal/voice"
)

// voiceTranscribedMsg carries the transcript produced from a voice recording.
type voiceTranscribedMsg struct{ text string }

// voiceErrorMsg carries an error from the recording/transcription pipeline.
type voiceErrorMsg struct{ err error }

// voiceTickMsg drives the "● REC m:ss" elapsed-time readout while recording.
type voiceTickMsg struct{}

// initVoice constructs the voice controller from config when voice prompting is
// enabled and a transcription API key is present. On any misconfiguration it
// logs a warning and leaves m.voice nil (the feature stays disabled and the
// hotkey falls through to the input field).
func (m *Model) initVoice() {
	if !m.cfg.Voice.Enabled {
		return
	}
	if strings.TrimSpace(m.cfg.VoiceControl.APIKey) == "" {
		if m.logger != nil {
			m.logger.Warn("voice prompting enabled but [voice_control] api_key is empty; disabling")
		}
		return
	}
	tr, err := voice.NewTranscriber(m.cfg.VoiceControl.TTSProviderName, m.cfg.VoiceControl.APIKey, m.cfg.Voice.Language)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("voice prompting disabled", "error", err)
		}
		return
	}
	rec := voice.NewSystemRecorder(m.cfg.Voice.Recorder, m.cfg.Voice.RecorderCmd, m.cfg.Voice.MaxSeconds)
	m.voice = voice.NewController(rec, tr)
	m.voiceHotkey = m.cfg.Voice.Hotkey
	if m.voiceHotkey == "" {
		m.voiceHotkey = "ctrl+r"
	}
}

// handleVoiceHotkey toggles voice recording. First press starts recording;
// second press stops it and kicks off transcription on a background command.
// While transcribing, further presses are ignored.
func (m Model) handleVoiceHotkey() (tea.Model, tea.Cmd) {
	if m.voice == nil {
		return m, nil
	}
	switch m.voice.State() {
	case voice.StateIdle:
		if err := m.voice.Start(); err != nil {
			m.voiceErr = strings.TrimPrefix(err.Error(), "voice: ")
			return m, nil
		}
		m.voiceErr = ""
		m.voiceStart = time.Now()
		return m, voiceTickCmd()
	case voice.StateRecording:
		path, err := m.voice.Stop()
		if err != nil {
			m.voiceErr = strings.TrimPrefix(err.Error(), "voice: ")
			return m, nil
		}
		// Transcribe off the event loop. Capture only the controller pointer
		// and the audio path (immutable values) — never the model.
		ctrl := m.voice
		return m, func() tea.Msg {
			text, terr := ctrl.Transcribe(context.Background(), path)
			if terr != nil {
				return voiceErrorMsg{err: terr}
			}
			return voiceTranscribedMsg{text: text}
		}
	}
	// StateTranscribing: ignore.
	return m, nil
}

// voiceTickCmd schedules a voiceTickMsg to refresh the recording timer.
func voiceTickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		return voiceTickMsg{}
	})
}

// voiceStatus renders the footer indicator for the current voice state, or ""
// when there is nothing to show.
func (m Model) voiceStatus() string {
	if m.voice != nil {
		switch m.voice.State() {
		case voice.StateRecording:
			d := time.Since(m.voiceStart)
			return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true).
				Render(fmt.Sprintf("● REC %d:%02d", int(d.Minutes()), int(d.Seconds())%60))
		case voice.StateTranscribing:
			return lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("⋯ transcribing…")
		}
	}
	if m.voiceErr != "" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("voice: " + m.voiceErr)
	}
	return ""
}
