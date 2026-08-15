// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// recorder tees the harness's narration to stdout and to an asciicast v2 file.
//
// Written here rather than shelling out to `asciinema` on purpose: the proof is
// supposed to be reproducible on any machine and in CI, and requiring a Python
// tool that is absent from most runners (including this project's) would make
// the recording the flaky part of a security test. asciicast v2 is a JSON
// header line followed by [time, "o", data] lines — cheap to emit exactly.
//
// Play it with:  asciinema play wire-test.cast
// or upload/embed it anywhere that renders asciicast.
type recorder struct {
	castPath string
	start    time.Time
	events   [][3]any
	failed   bool
}

func newRecorder() *recorder {
	return &recorder{start: time.Now()}
}

// step emits a headline beat of the run.
func (r *recorder) step(format string, args ...any) {
	r.emit("\n▶ " + fmt.Sprintf(format, args...) + "\n")
}

// printf emits a detail line.
func (r *recorder) printf(format string, args ...any) {
	r.emit("  " + fmt.Sprintf(format, args...) + "\n")
}

// failf records a failure. Kept visually distinct in the recording so a viewer
// cannot mistake a failed run for a passing one.
func (r *recorder) failf(format string, args ...any) {
	r.failed = true
	r.emit("\n✗ FAIL: " + fmt.Sprintf(format, args...) + "\n")
}

func (r *recorder) emit(s string) {
	fmt.Print(s)
	r.events = append(r.events, [3]any{time.Since(r.start).Seconds(), "o", s})
}

// finish writes the .cast file. A no-op when no path was set (the run died
// before it could choose an evidence dir).
func (r *recorder) finish() error {
	if r.castPath == "" {
		return nil
	}
	var b strings.Builder

	header := map[string]any{
		"version":   2,
		"width":     100,
		"height":    30,
		"timestamp": r.start.Unix(),
		"title":     "rysh wire test — planted secret never reaches the upstream",
		"env":       map[string]string{"SHELL": "/bin/sh", "TERM": "xterm-256color"},
	}
	h, err := json.Marshal(header)
	if err != nil {
		return err
	}
	b.Write(h)
	b.WriteByte('\n')

	for _, ev := range r.events {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(r.castPath, []byte(b.String()), 0o644)
}
