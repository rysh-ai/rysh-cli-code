package main

import (
	"log/slog"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"time"

	"github.com/fsnotify/fsnotify"
)

// promptReloadDebounce coalesces a burst of filesystem events (editors often
// emit several writes/renames per save) into a single reload trigger.
var promptReloadDebounce = 250 * time.Millisecond

// startPromptWatcher watches the prompt override directory for .md changes and,
// after a debounce, invokes onReload exactly once per settled burst. The CLI
// wires onReload to publish MsgReloadPromptsRequest to ws.inbox — which the
// active WorkspaceActor turns into the same reload + broadcast the ##agent
// reload-prompts command performs. Follow-up 2b (third-pass auto-reload on top
// of the existing SIGHUP + command paths).
//
// It is a no-op (returns nil, nil) when no override directory is configured, so
// the common "embedded prompts only" setup pays nothing. The returned stop
// function tears the watcher down; the daemon's process exit also releases it.
//
// onReload is decoupled from NATS so the debounce/filter logic is unit-testable.
func startPromptWatcher(dir string, onReload func()) (stop func(), err error) {
	if dir == "" || onReload == nil {
		return nil, nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	done := make(chan struct{})
	go func() {
		var timer *time.Timer
		var timerC <-chan time.Time
		arm := func() {
			if timer == nil {
				timer = time.NewTimer(promptReloadDebounce)
			} else {
				timer.Reset(promptReloadDebounce)
			}
			timerC = timer.C
		}
		for {
			select {
			case <-done:
				if timer != nil {
					timer.Stop()
				}
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				// Only react to prompt files. Catch writes, atomic-save creates,
				// and editor rename/remove churn.
				if filepath.Ext(ev.Name) != ".md" {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				arm()
			case watchErr, ok := <-w.Errors:
				if !ok {
					return
				}
				slog.Warn(progname.Rewrite("rysh: prompt watcher error"), "err", watchErr)
			case <-timerC:
				timerC = nil // disarm until the next event re-arms via timer.Reset
				slog.Info(progname.Rewrite("rysh: prompt override dir changed; reload triggered"), "dir", dir)
				onReload()
			}
		}
	}()

	return func() {
		close(done)
		_ = w.Close()
	}, nil
}
