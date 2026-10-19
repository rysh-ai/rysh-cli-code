// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// startIdleShell starts an interactive bash on a PTY exactly as startShell
// does — pty.Start, then pollablePTY — and exercises the ioctls the pane runs
// on every PTY (resize, foreground group), any of which could switch the
// master back to blocking mode.
func startIdleShell(t *testing.T) (*os.File, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command("/bin/bash", "--norc", "--noprofile", "-i")
	f, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("pty.Start unavailable: %v", err)
	}
	f = pollablePTY(f)
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = f.Close(); _ = cmd.Wait() })
	if err := setPTYSize(f, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatalf("setPTYSize: %v", err)
	}
	if foregroundPgrp(f) <= 0 {
		t.Fatal("foregroundPgrp could not read the foreground group")
	}
	return f, cmd
}

// drainUntilTimeout reads until a Read fails, re-arming a short deadline
// before each read, and reports the error — or nil if nothing returned.
func drainUntilTimeout(f *os.File, within time.Duration) error {
	done := make(chan error, 1)
	go func() {
		b := make([]byte, 4096)
		for {
			_ = f.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := f.Read(b); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		return nil
	}
}

// F-68: creack/pty's own ioctls go through Fd(), leaving the master blocking,
// so a read deadline never woke rawReadLoop on an idle pane — and its idle
// raw-publish flush and exit debounce never fired.
func TestPTYReadDeadlineWakesAnIdleRead(t *testing.T) {
	f, _ := startIdleShell(t)
	err := drainUntilTimeout(f, 5*time.Second)
	if err == nil {
		t.Fatal("a Read with a 200ms deadline on an idle PTY never returned — the master is in blocking mode")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read returned %v, want a deadline timeout", err)
	}
}

// Close must reach a Read already parked on the PTY: stopShell closes the
// master and relies on rawReadLoop exiting. On a blocking master it did not,
// so a pane whose terminal was still held open leaked its read loop.
func TestPTYCloseInterruptsAPendingRead(t *testing.T) {
	f, _ := startIdleShell(t)
	if err := drainUntilTimeout(f, 5*time.Second); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("could not reach an idle PTY: %v", err)
	}
	_ = f.SetReadDeadline(time.Time{})
	done := make(chan error, 1)
	go func() {
		b := make([]byte, 64)
		_, err := f.Read(b)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the Read park
	_ = f.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not interrupt a pending Read on the PTY master")
	}
}

func TestSetPTYSizeApplies(t *testing.T) {
	f, _ := startIdleShell(t)
	if err := setPTYSize(f, &pty.Winsize{Rows: 42, Cols: 123}); err != nil {
		t.Fatalf("setPTYSize: %v", err)
	}
	rc, _ := f.SyscallConn()
	var rows, cols uint16
	_ = rc.Control(func(fd uintptr) {
		if ws, err := unixGetWinsize(fd); err == nil {
			rows, cols = ws.Row, ws.Col
		}
	})
	if rows != 42 || cols != 123 {
		t.Fatalf("winsize = %dx%d, want 42x123", rows, cols)
	}
}

func unixGetWinsize(fd uintptr) (*unix.Winsize, error) {
	return unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
}
