// SPDX-License-Identifier: Apache-2.0

package tui

// Phase 2 (bash-shell-mode): readline key priority, reverse-i-search, and
// prefix-chord tests.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// readlineModel builds a Model with shell readline keys enabled and a pane
// carrying the given shell history.
func readlineModel(history []string) Model {
	m := paneModel(domain.PaneSnapshot{ID: "p1", ShellHistory: history})
	m.cfg = config.Config{ShellReadlineKeys: true}
	m.mode = modeNormal
	m.syncPaneInputs()
	return m
}

func keyRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// asModel unwraps a tea.Model returned by update paths that may yield either
// a Model value or a *Model pointer (pointer-receiver helpers return &m).
func asModel(t *testing.T, mm tea.Model) Model {
	t.Helper()
	switch v := mm.(type) {
	case Model:
		return v
	case *Model:
		return *v
	default:
		t.Fatalf("unexpected model type %T", mm)
		return Model{}
	}
}

func TestShellReadlineActiveGating(t *testing.T) {
	m := readlineModel(nil)
	if !m.shellReadlineActive() {
		t.Fatal("expected readline active in shell mode with config on")
	}
	m.setActiveInputMode("prompt")
	if m.shellReadlineActive() {
		t.Fatal("readline must not be active in prompt mode")
	}
	m.setActiveInputMode("shell")
	m.cfg.ShellReadlineKeys = false
	if m.shellReadlineActive() {
		t.Fatal("readline must not be active when config is off")
	}
	m.cfg.ShellReadlineKeys = true
	m.mode = modePane
	if m.shellReadlineActive() {
		t.Fatal("readline must not be active outside modeNormal")
	}
}

func TestOnlyCtrlRIsReserved(t *testing.T) {
	// rysh owns the Ctrl namespace: in shell mode, every Ctrl chord except
	// Ctrl+R must pass through UNhandled so the multiplexer switch (pane/tab/
	// layout modes, new pane, ...) receives it.
	m := readlineModel([]string{"echo one"})
	notReserved := []tea.KeyMsg{
		{Type: tea.KeyCtrlP},
		{Type: tea.KeyCtrlN},
		{Type: tea.KeyCtrlT},
		{Type: tea.KeyCtrlW},
		{Type: tea.KeyCtrlY},
		{Type: tea.KeyCtrlL},
		{Type: tea.KeyCtrlD},
		{Type: tea.KeyCtrlZ},
		{Type: tea.KeyCtrlA},
		{Type: tea.KeyCtrlE},
	}
	for _, k := range notReserved {
		if handled, _, _ := m.handleShellReadlineKey(k); handled {
			t.Errorf("%s must NOT be taken from the multiplexer in shell mode", k.String())
		}
	}
	handled, mm, _ := m.handleShellReadlineKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !handled {
		t.Fatal("ctrl+r must open reverse-i-search in shell mode")
	}
	if res := asModel(t, mm); !res.search.active {
		t.Error("ctrl+r should activate the search overlay")
	}
}

func TestFindNthMatch(t *testing.T) {
	hist := []string{"make build", "git status", "git push", "ls", "git push"}
	// newest-first scan: git push (dup collapses), git status
	if s, ok := findNthMatch(hist, "git", 0); !ok || s != "git push" {
		t.Errorf("n=0: got %q ok=%v, want 'git push'", s, ok)
	}
	if s, ok := findNthMatch(hist, "git", 1); !ok || s != "git status" {
		t.Errorf("n=1: got %q ok=%v, want 'git status'", s, ok)
	}
	if _, ok := findNthMatch(hist, "git", 2); ok {
		t.Error("n=2: expected no match")
	}
	if _, ok := findNthMatch(hist, "", 0); ok {
		t.Error("empty query must not match")
	}
}

func TestReverseSearchFlow(t *testing.T) {
	m := readlineModel([]string{"make test", "git commit -m x", "ls -la"})
	m.setActiveInputValue("draft")

	// Ctrl+R opens the search and saves the draft.
	handled, mm, _ := m.handleShellReadlineKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !handled {
		t.Fatal("ctrl+r should be handled")
	}
	m = asModel(t, mm)
	if !m.search.active || m.search.saved != "draft" {
		t.Fatalf("search not opened correctly: %+v", m.search)
	}

	// Typing narrows to the newest match.
	mm2, _ := m.updateReverseSearch(keyRunes("git"))
	m = asModel(t, mm2)
	if m.search.match != "git commit -m x" {
		t.Errorf("query 'git' matched %q", m.search.match)
	}

	// Esc keeps the match in the input line.
	mm3, _ := m.updateReverseSearch(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, mm3)
	if m.search.active {
		t.Error("esc should close the search")
	}
	if got := m.activeInputValue(); got != "git commit -m x" {
		t.Errorf("esc left input %q, want the match", got)
	}
}

func TestReverseSearchAbortRestoresDraft(t *testing.T) {
	m := readlineModel([]string{"make test"})
	m.setActiveInputValue("draft")
	_, mm, _ := m.handleShellReadlineKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = asModel(t, mm)
	mm2, _ := m.updateReverseSearch(keyRunes("make"))
	m = asModel(t, mm2)
	mm3, _ := m.updateReverseSearch(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = asModel(t, mm3)
	if m.search.active {
		t.Error("ctrl+g should close the search")
	}
	if got := m.activeInputValue(); got != "draft" {
		t.Errorf("ctrl+g left input %q, want restored draft", got)
	}
}

func TestReverseSearchFailedLabel(t *testing.T) {
	m := readlineModel([]string{"ls"})
	_, mm, _ := m.handleShellReadlineKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = asModel(t, mm)
	mm2, _ := m.updateReverseSearch(keyRunes("nomatch"))
	m = asModel(t, mm2)
	view := m.reverseSearchView(60)
	if want := "(failed reverse-i-search)"; len(view) == 0 || view[:len(want)] != want {
		t.Errorf("failed search view = %q, want prefix %q", view, want)
	}
}

func TestPrefixChordsEnterModes(t *testing.T) {
	cases := []struct {
		key  string
		want inputMode
	}{
		{"t", modeTab},
		{"p", modePane},
		{"w", modeWorkspace},
		{"s", modeStack},
		{"y", modeMovePane},
		{"l", modeLayout},
	}
	for _, tc := range cases {
		m := readlineModel(nil)
		m.mode = modePrefix
		mm, _ := m.updatePrefixMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		if got := asModel(t, mm).mode; got != tc.want {
			t.Errorf("ctrl+o %s → mode %v, want %v", tc.key, got, tc.want)
		}
	}
	// Unknown key still cancels the prefix.
	m := readlineModel(nil)
	m.mode = modePrefix
	mm, _ := m.updatePrefixMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if got := asModel(t, mm).mode; got != modeNormal {
		t.Errorf("ctrl+o q → mode %v, want modeNormal", got)
	}
}

func TestPrefixHistorySearch(t *testing.T) {
	// Phase 3: Up with a non-empty draft only visits entries with that prefix.
	m := readlineModel([]string{"git status", "make build", "git push", "ls"})
	m.setActiveInputValue("git")

	m.historyUp()
	if got := m.activeInputValue(); got != "git push" {
		t.Errorf("first Up = %q, want 'git push'", got)
	}
	m.historyUp()
	if got := m.activeInputValue(); got != "git status" {
		t.Errorf("second Up = %q, want 'git status'", got)
	}
	m.historyUp() // at oldest matching entry — stays
	if got := m.activeInputValue(); got != "git status" {
		t.Errorf("third Up = %q, want 'git status' (clamped)", got)
	}
	m.historyDown()
	if got := m.activeInputValue(); got != "git push" {
		t.Errorf("Down = %q, want 'git push'", got)
	}
	m.historyDown() // back to the draft
	if got := m.activeInputValue(); got != "git" {
		t.Errorf("Down to bottom = %q, want restored draft 'git'", got)
	}
}

func TestPlainHistoryWithoutPrefix(t *testing.T) {
	// Empty draft → plain full-history walk (unfiltered).
	m := readlineModel([]string{"git status", "ls"})
	m.historyUp()
	if got := m.activeInputValue(); got != "ls" {
		t.Errorf("Up = %q, want 'ls'", got)
	}
	m.historyUp()
	if got := m.activeInputValue(); got != "git status" {
		t.Errorf("Up = %q, want 'git status'", got)
	}
}
