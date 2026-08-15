// SPDX-License-Identifier: Apache-2.0

// Package replay implements session replay / time-travel (design 006): capture
// a session's pane output and export it as an asciicast (.cast) file so demos
// and incidents become reproducible artifacts.
//
// This package provides the asciicast v2 writer (pure), an in-session output
// Capture, and JetStream-backed durable capture (durable.go): a per-session
// stream on the pane output subject so export survives a daemon restart. Full
// scrubbing playback into a read-only replay pane is a later increment.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Header is the asciicast v2 header line.
type Header struct {
	Version   int    `json:"version"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Timestamp int64  `json:"timestamp,omitempty"` // unix seconds of the first event
	Title     string `json:"title,omitempty"`
}

// Event is one cast event: seconds since capture start, the event kind, and
// the data. Kind "" or "o" is terminal output; "r" is a mid-recording resize
// with Data "COLSxROWS" (asciicast v2 resize event).
type Event struct {
	Time float64
	Data string
	Kind string
}

// WriteCast writes an asciicast v2 document: a JSON header line followed by one
// [time, kind, data] JSON array per event ("o" output, "r" resize). Playable by
// `asciinema play`.
func WriteCast(w io.Writer, h Header, events []Event) error {
	if h.Version == 0 {
		h.Version = 2
	}
	if h.Width == 0 {
		h.Width = 80
	}
	if h.Height == 0 {
		h.Height = 24
	}
	bw := bufio.NewWriter(w)
	hb, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := bw.Write(hb); err != nil {
		return err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return err
	}
	for _, e := range events {
		kind := e.Kind
		if kind == "" {
			kind = "o"
		}
		line, err := json.Marshal([]interface{}{e.Time, kind, e.Data})
		if err != nil {
			return err
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("replay: flush cast: %w", err)
	}
	return nil
}
