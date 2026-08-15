// SPDX-License-Identifier: Apache-2.0

package webauto

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

// TestResolveRecordPrecedence pins the tier order that every other run knob in
// this package follows: flags > recipe > config > built-ins.
func TestResolveRecordPrecedence(t *testing.T) {
	// Nothing set anywhere → off, with the built-in capture settings.
	s := ResolveRecord(nil, nil, RecordFlags{})
	if s.Enabled {
		t.Error("recording should default to off")
	}
	if s.Interval != DefaultRecordInterval || s.Format != DefaultRecordFormat ||
		s.Quality != DefaultRecordQuality || s.MaxFrames != DefaultRecordMaxFrames {
		t.Errorf("built-in defaults wrong: %+v", s)
	}

	// Config alone can turn it on.
	s = ResolveRecord(nil, &RecordConfig{Enabled: boolPtr(true), Interval: "2s"}, RecordFlags{})
	if !s.Enabled || s.Interval != 2*time.Second {
		t.Errorf("config tier ignored: %+v", s)
	}

	// Recipe beats config, field by field: it overrides the interval but
	// leaves the format the config set.
	s = ResolveRecord(
		&RecordConfig{Interval: "250ms"},
		&RecordConfig{Enabled: boolPtr(true), Interval: "2s", Format: "png"},
		RecordFlags{})
	if !s.Enabled || s.Interval != 250*time.Millisecond || s.Format != "png" {
		t.Errorf("recipe should beat config per-field: %+v", s)
	}

	// Flags beat both.
	s = ResolveRecord(
		&RecordConfig{Enabled: boolPtr(true), Interval: "250ms"},
		&RecordConfig{Interval: "2s"},
		RecordFlags{Interval: 100 * time.Millisecond})
	if s.Interval != 100*time.Millisecond {
		t.Errorf("flag interval should win: %+v", s)
	}
}

// TestResolveRecordEnableDisable covers the on/off asymmetry: --no-record has
// to be able to switch off a recipe that records by default, and a bare
// --recording-path implies the user wants a recording.
func TestResolveRecordEnableDisable(t *testing.T) {
	recipeOn := &RecordConfig{Enabled: boolPtr(true)}

	if s := ResolveRecord(recipeOn, nil, RecordFlags{Off: true}); s.Enabled {
		t.Error("--no-record must override a recipe that enables recording")
	}
	if s := ResolveRecord(nil, nil, RecordFlags{On: true}); !s.Enabled {
		t.Error("--record must enable recording")
	}
	// Naming an output path is an implicit --record.
	s := ResolveRecord(nil, nil, RecordFlags{Path: "run.mp4"})
	if !s.Enabled || s.Path != "run.mp4" {
		t.Errorf("--recording-path should imply --record: %+v", s)
	}
	// Contradictory flags fail safe (off), regardless of order.
	if s := ResolveRecord(nil, nil, RecordFlags{On: true, Off: true}); s.Enabled {
		t.Error("--record --no-record must resolve to off")
	}
	// A recipe that explicitly disables beats a config that enables.
	s = ResolveRecord(&RecordConfig{Enabled: boolPtr(false)}, &RecordConfig{Enabled: boolPtr(true)}, RecordFlags{})
	if s.Enabled {
		t.Error("recipe enabled:false should beat config enabled:true")
	}
}

// TestResolveRecordClampsAndFormats covers input sanitising: sub-floor
// intervals, junk durations, unknown formats, out-of-range quality.
func TestResolveRecordClampsAndFormats(t *testing.T) {
	if s := ResolveRecord(nil, nil, RecordFlags{Interval: time.Millisecond}); s.Interval != MinRecordInterval {
		t.Errorf("interval should clamp to %s, got %s", MinRecordInterval, s.Interval)
	}
	// Unparseable/invalid values leave the tier below intact.
	s := ResolveRecord(&RecordConfig{Interval: "banana", Format: "gif", Quality: 500}, nil, RecordFlags{})
	if s.Interval != DefaultRecordInterval || s.Format != DefaultRecordFormat || s.Quality != DefaultRecordQuality {
		t.Errorf("invalid values should not overwrite defaults: %+v", s)
	}
	// Format spellings normalise.
	for in, want := range map[string]string{"JPG": "jpeg", "jpeg": "jpeg", "PNG": "png", "webp": "webp"} {
		if got := ResolveRecord(&RecordConfig{Format: in}, nil, RecordFlags{}).Format; got != want {
			t.Errorf("format %q → %q, want %q", in, got, want)
		}
	}
	if got := (RecordSpec{Format: "jpeg"}).FrameExt(); got != "jpg" {
		t.Errorf("jpeg frame ext = %q, want jpg", got)
	}
}

// TestRecordResolvePath covers the four path shapes: default, relative file,
// absolute file, and directory-like (which keeps the generated filename).
func TestRecordResolvePath(t *testing.T) {
	at := time.Date(2026, 7, 21, 14, 30, 5, 0, time.UTC)
	out := filepath.Join("/tmp", "results")

	got := RecordSpec{}.ResolvePath(out, "guest scout", at)
	want := filepath.Join(out, "recordings", SanitizeName("guest scout")+"-20260721-143005.mp4")
	if got != want {
		t.Errorf("default path = %q, want %q", got, want)
	}

	// Relative file → anchored under the output dir.
	if got := (RecordSpec{Path: "clips/run.mp4"}).ResolvePath(out, "scout", at); got != filepath.Join(out, "clips", "run.mp4") {
		t.Errorf("relative path = %q", got)
	}
	// Absolute file → used as-is.
	if got := (RecordSpec{Path: "/videos/run.mp4"}).ResolvePath(out, "scout", at); got != "/videos/run.mp4" {
		t.Errorf("absolute path = %q", got)
	}
	// Directory-like (no extension) → generated filename inside it.
	got = RecordSpec{Path: "/videos/scout"}.ResolvePath(out, "scout", at)
	if !strings.HasPrefix(got, "/videos/scout/") || !strings.HasSuffix(got, ".mp4") {
		t.Errorf("dir-like path = %q, want a .mp4 inside /videos/scout", got)
	}
}
