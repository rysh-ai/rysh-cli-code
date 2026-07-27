// Package voice implements voice prompting for the rysh TUI: it records the
// microphone via a system recorder, transcribes the audio with a pluggable
// speech-to-text provider (Deepgram by default, OpenAI Whisper as an
// alternative), and hands the text back to the caller to drop into the prompt
// input field.
package voice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Recorder captures microphone audio to a temporary file.
//
// The lifecycle is: Start -> (Stop | Cancel). Start begins capture; Stop
// finalizes the recording and returns the path to the captured audio file;
// Cancel aborts and deletes any temp file. Implementations must be safe to
// reuse for a subsequent Start after Stop/Cancel.
type Recorder interface {
	// Start begins capturing audio. It returns an error if no recorder tool
	// is available or the capture process fails to launch.
	Start(ctx context.Context) error
	// Stop finalizes the recording and returns the path to the audio file.
	// The caller owns the file and is responsible for deleting it.
	Stop() (audioPath string, err error)
	// Cancel aborts an in-progress recording and removes the temp file.
	Cancel()
	// Recording reports whether a capture is currently in progress.
	Recording() bool
}

// systemRecorder records audio by shelling out to a system recorder tool
// (sox/rec, ffmpeg, afrecord, or arecord). It writes 16 kHz mono WAV, which is
// what the transcription providers expect.
type systemRecorder struct {
	// tool is the configured recorder selector: "auto", "sox", "ffmpeg",
	// "afrecord", or "arecord".
	tool string
	// override is an optional full command template. When set it takes
	// precedence over tool detection. The placeholder {output} is replaced
	// with the temp WAV path.
	override string
	// maxSeconds caps a single recording. 0 disables the cap.
	maxSeconds int

	cmd     *exec.Cmd
	tmpPath string
	done    chan struct{}
}

// NewSystemRecorder constructs a Recorder backed by a system recording tool.
//   - recorder selects the tool ("auto" auto-detects per OS).
//   - overrideCmd is an optional full command template using {output}.
//   - maxSeconds caps a recording (0 = no cap).
func NewSystemRecorder(recorder, overrideCmd string, maxSeconds int) Recorder {
	if recorder == "" {
		recorder = "auto"
	}
	return &systemRecorder{
		tool:       strings.ToLower(strings.TrimSpace(recorder)),
		override:   strings.TrimSpace(overrideCmd),
		maxSeconds: maxSeconds,
	}
}

func (r *systemRecorder) Recording() bool { return r.cmd != nil }

func (r *systemRecorder) Start(ctx context.Context) error {
	if r.cmd != nil {
		return errors.New("voice: already recording")
	}

	tmp, err := os.CreateTemp("", "rysh-voice-*.wav")
	if err != nil {
		return fmt.Errorf("voice: create temp file: %w", err)
	}
	r.tmpPath = tmp.Name()
	_ = tmp.Close()

	name, args, err := r.buildCommand(r.tmpPath)
	if err != nil {
		os.Remove(r.tmpPath)
		r.tmpPath = ""
		return err
	}

	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		os.Remove(r.tmpPath)
		r.tmpPath = ""
		return fmt.Errorf("voice: start %s: %w", name, err)
	}
	r.cmd = cmd
	r.done = make(chan struct{})
	go func(c *exec.Cmd, done chan struct{}) {
		_ = c.Wait()
		close(done)
	}(cmd, r.done)
	return nil
}

func (r *systemRecorder) Stop() (string, error) {
	if r.cmd == nil {
		return "", errors.New("voice: not recording")
	}
	r.signalAndWait(5 * time.Second)
	path := r.tmpPath
	r.cmd = nil
	r.tmpPath = ""

	// A recorder that never received audio (e.g. denied microphone
	// permission, or a wrong device) often produces a tiny or empty file.
	// Treat anything below a WAV header + a sliver of samples as "no audio".
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("voice: recording missing: %w", err)
	}
	if fi.Size() < 1024 {
		os.Remove(path)
		return "", errors.New("voice: no audio captured (check microphone access/permission)")
	}
	return path, nil
}

func (r *systemRecorder) Cancel() {
	if r.cmd != nil {
		r.signalAndWait(2 * time.Second)
		r.cmd = nil
	}
	if r.tmpPath != "" {
		os.Remove(r.tmpPath)
		r.tmpPath = ""
	}
}

// signalAndWait asks the recorder process to finish (SIGINT, which makes
// sox/ffmpeg/arecord flush and finalize the WAV header), then waits up to
// timeout for it to exit, killing it as a last resort.
func (r *systemRecorder) signalAndWait(timeout time.Duration) {
	if r.cmd == nil || r.cmd.Process == nil {
		return
	}
	_ = r.cmd.Process.Signal(os.Interrupt)
	select {
	case <-r.done:
	case <-time.After(timeout):
		_ = r.cmd.Process.Kill()
		<-r.done
	}
}

// buildCommand resolves the recorder tool and returns the command + args that
// record 16 kHz mono WAV to out.
func (r *systemRecorder) buildCommand(out string) (string, []string, error) {
	if r.override != "" {
		fields := strings.Fields(strings.ReplaceAll(r.override, "{output}", out))
		if len(fields) == 0 {
			return "", nil, errors.New("voice: recorder_cmd is empty")
		}
		return fields[0], fields[1:], nil
	}

	tool := r.tool
	if tool == "" || tool == "auto" {
		tool = detectRecorder()
		if tool == "" {
			return "", nil, errors.New("voice: no recorder found — install sox or ffmpeg (macOS) / arecord or sox (linux)")
		}
	} else if _, err := exec.LookPath(tool); err != nil {
		return "", nil, fmt.Errorf("voice: configured recorder %q not found in PATH", tool)
	}

	switch tool {
	case "rec":
		args := []string{"-q", "-c", "1", "-r", "16000", out}
		if r.maxSeconds > 0 {
			args = append(args, "trim", "0", strconv.Itoa(r.maxSeconds))
		}
		return "rec", args, nil
	case "sox":
		args := []string{"-d", "-q", "-c", "1", "-r", "16000", out}
		if r.maxSeconds > 0 {
			args = append(args, "trim", "0", strconv.Itoa(r.maxSeconds))
		}
		return "sox", args, nil
	case "ffmpeg":
		format, input := ffmpegInput()
		args := []string{"-hide_banner", "-loglevel", "error", "-f", format, "-i", input, "-ac", "1", "-ar", "16000"}
		if r.maxSeconds > 0 {
			args = append(args, "-t", strconv.Itoa(r.maxSeconds))
		}
		args = append(args, "-y", out)
		return "ffmpeg", args, nil
	case "arecord":
		args := []string{"-q", "-f", "S16_LE", "-c", "1", "-r", "16000"}
		if r.maxSeconds > 0 {
			args = append(args, "-d", strconv.Itoa(r.maxSeconds))
		}
		args = append(args, out)
		return "arecord", args, nil
	case "afrecord":
		// macOS Core Audio recorder. afrecord records until interrupted.
		return "afrecord", []string{"-f", "WAVE", "-d", "LEI16@16000", "-c", "1", out}, nil
	default:
		return "", nil, fmt.Errorf("voice: unsupported recorder %q", tool)
	}
}

// detectRecorder picks the first available recorder tool for the current OS.
func detectRecorder() string {
	var order []string
	switch runtime.GOOS {
	case "darwin":
		order = []string{"rec", "sox", "ffmpeg", "afrecord"}
	case "linux":
		order = []string{"arecord", "rec", "sox", "ffmpeg"}
	default:
		order = []string{"ffmpeg", "sox", "rec"}
	}
	for _, t := range order {
		if _, err := exec.LookPath(t); err == nil {
			return t
		}
	}
	return ""
}

// ffmpegInput returns the (-f format, -i input) pair for capturing the default
// microphone with ffmpeg on the current OS.
func ffmpegInput() (format, input string) {
	switch runtime.GOOS {
	case "darwin":
		// avfoundation: "video:audio"; ":default" selects the default audio
		// device with no video.
		return "avfoundation", ":default"
	case "linux":
		return "alsa", "default"
	default:
		return "alsa", "default"
	}
}
