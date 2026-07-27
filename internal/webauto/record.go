package webauto

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Run recording (`##auto web run --record`) captures a browser screenshot on a
// fixed interval for the whole run and encodes the frames into one video. It
// is a web-kind-only feature: the other ##auto kinds have no browser to shoot.
//
// Precedence mirrors every other run knob in this package —
//
//	run flags > recipe frontmatter (`record:`) > config (`automation.web.record`) > built-ins
//
// so a recipe can record by default and a single run can override the path, or
// opt out entirely with --no-record.

// Built-in defaults. The interval is the headline feature (a frame every half
// second); the rest exist to keep a long run from filling the disk. A full-size
// PNG frame runs 200-500 KB, so a 10-minute run at 2 fps would cost ~500 MB —
// JPEG at quality 60 lands the same run near 50 MB.
const (
	DefaultRecordInterval  = 500 * time.Millisecond
	DefaultRecordFormat    = "jpeg"
	DefaultRecordQuality   = 60
	DefaultRecordMaxFrames = 7200 // ~1h at the default interval
	// MinRecordInterval floors the tick. Below this the capture round-trip
	// (CDP screenshot on a heavy page) never keeps up and every other frame
	// is dropped anyway, while the CDP traffic starts contending with the
	// agent's own browser actions.
	MinRecordInterval = 100 * time.Millisecond
)

// RecordConfig is the `record:` block, shared by recipe frontmatter and the
// `automation.web.record` config section:
//
//	record:
//	  enabled: true
//	  path: recordings/run.mp4   # relative → under the recipe's output_dir
//	  interval: 500ms
//	  format: jpeg               # jpeg | png | webp
//	  quality: 60                # jpeg/webp only
//	  max_frames: 7200
type RecordConfig struct {
	Enabled   *bool  `yaml:"enabled,omitempty"`
	Path      string `yaml:"path,omitempty"`
	Interval  string `yaml:"interval,omitempty"`
	Format    string `yaml:"format,omitempty"`
	Quality   int    `yaml:"quality,omitempty"`
	MaxFrames int    `yaml:"max_frames,omitempty"`
}

// RecordFlags is the run-flag tier: --record / --no-record / --recording-path /
// --record-interval. On and Off are separate booleans (not one tristate) so
// "flag absent" stays distinguishable from "flag set false" — --no-record has
// to beat a recipe that enables recording.
type RecordFlags struct {
	On       bool
	Off      bool
	Path     string
	Interval time.Duration
}

// RecordSpec is the fully resolved plan the recorder actor runs on.
type RecordSpec struct {
	Enabled   bool
	Path      string // may be empty/relative here; see ResolvePath
	Interval  time.Duration
	Format    string
	Quality   int
	MaxFrames int
}

// ResolveRecord folds the three tiers into one spec. recipe and cfg may be nil.
func ResolveRecord(recipe, cfg *RecordConfig, f RecordFlags) RecordSpec {
	s := RecordSpec{
		Interval:  DefaultRecordInterval,
		Format:    DefaultRecordFormat,
		Quality:   DefaultRecordQuality,
		MaxFrames: DefaultRecordMaxFrames,
	}
	// Lowest tier first, each overwriting only the fields it actually sets.
	for _, c := range []*RecordConfig{cfg, recipe} {
		if c == nil {
			continue
		}
		if c.Enabled != nil {
			s.Enabled = *c.Enabled
		}
		if p := strings.TrimSpace(c.Path); p != "" {
			s.Path = p
		}
		if d, err := time.ParseDuration(strings.TrimSpace(c.Interval)); err == nil && d > 0 {
			s.Interval = d
		}
		if fm := normalizeRecordFormat(c.Format); fm != "" {
			s.Format = fm
		}
		if c.Quality > 0 && c.Quality <= 100 {
			s.Quality = c.Quality
		}
		if c.MaxFrames > 0 {
			s.MaxFrames = c.MaxFrames
		}
	}
	// Flags win. Off beats On so `--record --no-record` fails safe (off).
	if f.On {
		s.Enabled = true
	}
	if p := strings.TrimSpace(f.Path); p != "" {
		s.Path = p
		s.Enabled = true // naming an output path implies wanting the recording
	}
	if f.Interval > 0 {
		s.Interval = f.Interval
	}
	if f.Off {
		s.Enabled = false
	}
	if s.Interval < MinRecordInterval {
		s.Interval = MinRecordInterval
	}
	return s
}

// normalizeRecordFormat maps user spellings onto the CDP format names,
// returning "" for anything unrecognised (so the caller keeps its default).
func normalizeRecordFormat(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "jpeg", "jpg":
		return "jpeg"
	case "png":
		return "png"
	case "webp":
		return "webp"
	}
	return ""
}

// FrameExt is the file extension for the spec's capture format.
func (s RecordSpec) FrameExt() string {
	if s.Format == "jpeg" {
		return "jpg"
	}
	return s.Format
}

// ResolvePath returns the absolute video path for a run. An empty Path
// defaults to <outputDir>/recordings/<recipe>-<timestamp>.mp4. A relative
// Path is anchored under outputDir; an absolute one is used as-is. A Path
// naming a directory (trailing separator, or no file extension) keeps the
// default filename inside it.
//
// startedAt is passed in rather than read from the clock so the caller can
// stamp the frames directory and the video with the same instant.
func (s RecordSpec) ResolvePath(outputDir, recipeName string, startedAt time.Time) string {
	stamp := startedAt.Format("20060102-150405")
	base := fmt.Sprintf("%s-%s.mp4", SanitizeName(recipeName), stamp)

	p := strings.TrimSpace(s.Path)
	if p == "" {
		return filepath.Join(outputDir, "recordings", base)
	}
	dirLike := strings.HasSuffix(p, string(filepath.Separator)) || filepath.Ext(p) == ""
	if !filepath.IsAbs(p) {
		p = filepath.Join(outputDir, p)
	}
	if dirLike {
		return filepath.Join(p, base)
	}
	return p
}

// Describe renders the one-line run-report summary for the recording plan.
func (s RecordSpec) Describe(path string) string {
	q := ""
	if s.Format == "jpeg" || s.Format == "webp" {
		q = fmt.Sprintf(" q%d", s.Quality)
	}
	return fmt.Sprintf("every %s, %s%s → %s", s.Interval, s.Format, q, path)
}
