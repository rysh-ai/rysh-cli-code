// SPDX-License-Identifier: Apache-2.0

//go:build windows

package actors

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/rysh-ai/rysh-cli-code/internal/conpty"
)

// On Windows a "process group id" in this package is a process id: the pane's
// shell, or the program the shell started. There is no TIOCGPGRP, so the
// foreground program is read from a Toolhelp process snapshot (foregroundChild),
// and a stop ends the program's whole process tree (processTree) because there
// is no group to signal.

// foregroundCacheFor bounds how often a pane takes a process snapshot.
// foregroundPgrp runs on every output chunk as well as from the 500ms poller,
// and a snapshot of every process on the machine is far dearer than an ioctl.
const foregroundCacheFor = 200 * time.Millisecond

var foregroundCache struct {
	sync.Mutex
	byShell map[int]foregroundSample
}

type foregroundSample struct {
	at  time.Time
	pid int
}

// foregroundPgrp returns the pid of the pane's foreground program — the newest
// child of the shell, or the shell itself at its prompt — or -1 if it cannot
// be determined.
func foregroundPgrp(t paneTerm) int {
	c, ok := t.(*conpty.Console)
	if !ok || c == nil {
		return -1
	}
	shell := c.Pid()
	if shell <= 0 {
		return -1
	}
	foregroundCache.Lock()
	defer foregroundCache.Unlock()
	if s, ok := foregroundCache.byShell[shell]; ok && time.Since(s.at) < foregroundCacheFor {
		return s.pid
	}
	procs, err := snapshotProcs(func(e snapProc) bool { return e.PID == shell || e.PPID == shell })
	pid := -1
	if err == nil {
		pid = foregroundChild(shell, procs)
	}
	if foregroundCache.byShell == nil {
		foregroundCache.byShell = map[int]foregroundSample{}
	}
	// A pane whose shell has gone stops being sampled; drop it so the map does
	// not grow for the life of the daemon.
	if pid < 0 {
		delete(foregroundCache.byShell, shell)
	} else {
		foregroundCache.byShell[shell] = foregroundSample{at: time.Now(), pid: pid}
	}
	return pid
}

// snapshotProcs lists the processes on the machine, reading the creation time
// only of those keep selects (a creation time costs an OpenProcess each).
// Rows keep does not select are returned with Created 0; a selected row whose
// time cannot be read gets -1, so the tree helpers treat it as a stranger.
func snapshotProcs(keep func(snapProc) bool) ([]snapProc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var procs []snapProc
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		e := snapProc{PID: int(pe.ProcessID), PPID: int(pe.ParentProcessID)}
		if keep(e) {
			e.Created = processCreated(pe.ProcessID)
		}
		procs = append(procs, e)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return procs, nil
}

// processCreated is a process's creation time in 100ns units, or -1.
func processCreated(pid uint32) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return -1
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return -1
	}
	return created.Nanoseconds() / 100
}

// processPgid returns pid unchanged on Windows (no process groups).
func processPgid(pid int) int { return pid }

// processGroupAlive reports whether the process is still running. The Unix
// version asks about a whole group; here the program's pid stands for it,
// which is what keeps a pane interactive while the program runs.
func processGroupAlive(pid int) bool {
	if pid <= 4 { // 0 is the idle process and 4 the System process
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means it exists; anything else, that it does not.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// terminateProcessGroup stops a program for `[proxy] strict` and `##ansa
// kill`. Windows has no SIGTERM to send a console program from outside its
// console, so this is the same hard stop as killProcessGroup: every process in
// the program's tree is terminated. The callers' guard (never the shell's own
// pid) is what keeps the pane itself alive.
func terminateProcessGroup(pid int) error { return terminateTree(pid) }

// killProcessGroup is terminateProcessGroup's harder sibling on Unix; on
// Windows both are the same tree stop. Separate function rather than a flag so
// a grep for the hard stop finds every site that can send one.
func killProcessGroup(pid int) error { return terminateTree(pid) }

func terminateTree(pid int) error {
	if pid <= 4 {
		return fmt.Errorf("refusing to stop pid %d", pid)
	}
	procs, err := snapshotProcs(func(snapProc) bool { return true })
	if err != nil {
		return err
	}
	tree := processTree(pid, procs)
	if len(tree) == 0 {
		return fmt.Errorf("process %d is not running", pid)
	}
	var rootErr error
	for i, p := range tree { // root first, so it cannot start more children
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p))
		if err == nil {
			err = windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
		}
		if i == 0 {
			rootErr = err
		}
	}
	return rootErr
}
