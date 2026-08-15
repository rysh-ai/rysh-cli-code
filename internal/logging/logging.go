// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"time"

	"github.com/lmittmann/tint"
)

// levelOff is a sentinel slog level above all real levels. A handler configured
// with this level discards every record, so the logger becomes a no-op.
const levelOff = slog.Level(1000)

// Setup configures the global slog logger based on the given log level.
//
// Supported levels: "off", "debug", "info", "warn", "error".
//
// Logging is DISABLED by default: when level is "off", "none" or empty, every
// log call becomes a no-op (records are discarded). This keeps the terminal UI
// clean unless the user explicitly opts in via --log-level or rysh.config.yaml.
//
// When level is "debug", logs are written to /tmp/rysh/logs/ in addition to
// stderr. The log file is named rysh-<session>-<timestamp>.log.
//
// For "info", "warn" and "error", logs go to stderr only (colorized via tint).
func Setup(level, sessionName string) *slog.Logger {
	if disabled(level) {
		// No-op logger: discard everything.
		logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: levelOff}))
		slog.SetDefault(logger)
		return logger
	}

	slogLevel := parseLevel(level)

	if slogLevel == slog.LevelDebug {
		return setupDebugLogger(sessionName)
	}

	// Non-debug: colorized stderr output via tint.
	logger := slog.New(tint.NewHandler(os.Stderr, &tint.Options{
		Level: slogLevel,
	}))
	slog.SetDefault(logger)
	return logger
}

// setupDebugLogger creates a logger that writes to both stderr (with tint) and
// a file under /tmp/rysh/logs/.
func setupDebugLogger(sessionName string) *slog.Logger {
	logDir := "/tmp/rysh/logs"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		// Fallback to stderr-only if we can't create the log directory.
		fmt.Fprintf(os.Stderr, progname.Rewrite("rysh: warning: cannot create log dir %s: %v\n"), logDir, err)
		logger := slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelDebug}))
		slog.SetDefault(logger)
		return logger
	}

	ts := time.Now().Format("20060102-150405")
	if sessionName == "" {
		sessionName = "default"
	}
	logFile := filepath.Join(logDir, fmt.Sprintf("rysh-%s-%s.log", sessionName, ts))

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, progname.Rewrite("rysh: warning: cannot open log file %s: %v\n"), logFile, err)
		logger := slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelDebug}))
		slog.SetDefault(logger)
		return logger
	}

	// Tee writer: both file and stderr get all log output.
	multi := io.MultiWriter(os.Stderr, f)
	logger := slog.New(tint.NewHandler(multi, &tint.Options{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	logger.Info("debug logging enabled", "log_file", logFile, "session", sessionName)

	return logger
}

// disabled reports whether the given level means "logging off".
// Empty, "off" and "none" all disable logging.
func disabled(level string) bool {
	switch level {
	case "", "off", "none":
		return true
	default:
		return false
	}
}

// parseLevel converts a string log level name to slog.Level.
// Defaults to slog.LevelInfo for unrecognized values.
func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ValidLevel returns true if the level string is a recognized log level.
// "off" and "none" are accepted as explicit ways to disable logging.
func ValidLevel(level string) bool {
	switch level {
	case "off", "none", "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
