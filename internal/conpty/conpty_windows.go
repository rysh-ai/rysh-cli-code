// SPDX-License-Identifier: Apache-2.0

//go:build windows

package conpty

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Console is a pseudo-console with a shell attached.
//
// Read returns what the shell draws (VT sequences, \r\n line ends); Write
// feeds it keystrokes — Enter is \r and Ctrl-C is \x03, which ConPTY turns
// into CTRL_C_EVENT for the attached processes, so the pane's existing input
// path needs no translation.
type Console struct {
	in  *os.File // write end of the console's input pipe
	out *os.File // read end of the console's output pipe (overlapped, pollable)
	pid int

	// job is guarded by mu and zeroed by Close: a Kill after Close must not
	// reach TerminateJobObject with a handle value Windows may have reused.
	// hpc is guarded the same way and zeroed when the console is released
	// (after the shell exits, or by Close): an HPCON is a pointer, so a Resize
	// after the release would use freed memory.
	mu  sync.Mutex
	job windows.Handle
	hpc windows.Handle

	// hpcRelease closes the pseudo-console once: watchExit does it when the
	// shell exits, which breaks the output pipe so the pane's Read ends with
	// io.EOF; Close does it if the pane goes first.
	hpcRelease releaser

	// The process handle belongs to reap alone: it waits on it, records the
	// exit code, closes it and then closes done. Close never touches it, so
	// Wait and Close cannot race on a handle one of them has just closed.
	done     chan struct{}
	exitCode uint32
	exitErr  error

	closeOnce sync.Once
}

// available reports whether this Windows has ConPTY. The x/sys wrappers call
// the proc's Addr, which panics when kernel32 lacks it (before 10 1809).
func available() bool {
	return windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole").Find() == nil
}

func coord(s Size) windows.Coord { return windows.Coord{X: int16(s.Cols), Y: int16(s.Rows)} }

// Start runs argv attached to a new pseudo-console of the given size.
//
// argv[0] must be a full path (ResolveShell returns one); no search is done.
// env is the complete environment (MergeEnv builds it). The process is put in
// a kill-on-close Job Object before it runs, so closing the Console ends the
// whole tree — the Windows form of signalling the pane's process group.
func Start(argv []string, dir string, env []string, size Size) (*Console, error) {
	if len(argv) == 0 {
		return nil, errors.New("conpty: empty argv")
	}
	if _, err := NewSize(int(size.Rows), int(size.Cols)); err != nil {
		return nil, err
	}
	if !available() {
		return nil, ErrUnsupported
	}
	block, err := EnvBlock(env)
	if err != nil {
		return nil, err
	}
	app, err := windows.UTF16PtrFromString(argv[0])
	if err != nil {
		return nil, err
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return nil, err
	}
	var cwd *uint16
	if dir != "" {
		if cwd, err = windows.UTF16PtrFromString(dir); err != nil {
			return nil, err
		}
	}

	var inRead, inWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("conpty: input pipe: %w", err)
	}
	outRead, outWrite, err := outputPipe()
	if err != nil {
		closeHandles(inRead, inWrite)
		return nil, fmt.Errorf("conpty: output pipe: %w", err)
	}
	var hpc windows.Handle
	err = windows.CreatePseudoConsole(coord(size), inRead, outWrite, 0, &hpc)
	// The pseudo-console holds its own references to its ends of the pipes.
	closeHandles(inRead, outWrite)
	if err != nil {
		closeHandles(inWrite, outRead)
		return nil, fmt.Errorf("conpty: CreatePseudoConsole: %w", err)
	}
	c := &Console{
		hpc:  hpc,
		in:   os.NewFile(uintptr(inWrite), "conpty-in"),
		out:  os.NewFile(uintptr(outRead), "conpty-out"),
		done: make(chan struct{}),
	}
	c.hpcRelease.release = c.releaseConsole
	if err := c.spawn(app, cmdline, cwd, block); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Console) spawn(app, cmdline, cwd *uint16, block []uint16) error {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("conpty: attribute list: %w", err)
	}
	defer attrs.Delete()
	// The attribute's value is the HPCON itself, not a pointer to it; the
	// double conversion reinterprets the handle's bits without vet's
	// uintptr-to-pointer warning.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&c.hpc)), unsafe.Sizeof(c.hpc)); err != nil {
		return fmt.Errorf("conpty: attach console: %w", err)
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("conpty: job object: %w", err)
	}
	c.job = job
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("conpty: job limits: %w", err)
	}

	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// Invalid std handles, explicitly: the rysh daemon runs with its stdio
	// redirected to a log, and without STARTF_USESTDHANDLES the shell can end up
	// with those handles instead of the pseudo-console's, writing around it.
	// INVALID_HANDLE_VALUE makes the console supply all three; wezterm's
	// portable-pty does the same for its daemonised mux server.
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = windows.InvalidHandle
	si.StdOutput = windows.InvalidHandle
	si.StdErr = windows.InvalidHandle
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT |
		windows.CREATE_SUSPENDED)
	if err := windows.CreateProcess(app, cmdline, nil, nil, false, flags, &block[0], cwd,
		&si.StartupInfo, &pi); err != nil {
		return fmt.Errorf("conpty: CreateProcess: %w", err)
	}
	defer closeHandles(pi.Thread)
	c.pid = int(pi.ProcessId)
	// Into the job while suspended, so not even the shell's first child can
	// escape it.
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		closeHandles(pi.Process)
		return fmt.Errorf("conpty: assign job: %w", err)
	}
	// From here the job owns the process: closing the job (Close, or Start's
	// error path) kills it, and reap sees it exit. watchExit then releases the
	// console so the output pipe ends.
	go c.reap(pi.Process)
	go watchExit(c.done, &c.hpcRelease)
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		return fmt.Errorf("conpty: resume: %w", err)
	}
	return nil
}

// reap waits for the shell to exit, records how, and releases its handle.
func (c *Console) reap(process windows.Handle) {
	defer close(c.done)
	defer closeHandles(process)
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		c.exitErr = err
		return
	}
	c.exitErr = windows.GetExitCodeProcess(process, &c.exitCode)
}

// outputPipe makes the pipe the pseudo-console writes to. It is a named pipe
// rather than CreatePipe's anonymous one so the read end can be overlapped:
// os.NewFile then registers it with Go's I/O completion port, which is what
// makes SetReadDeadline work (rawReadLoop's idle flush and exit debounce run on
// read deadlines, F-68 on Unix) and lets Close cancel a pending Read. The write
// end, which the console holds, stays synchronous. One instance, local only,
// with a random name and the default (owner-only) security descriptor.
func outputPipe() (read, write windows.Handle, err error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return 0, 0, err
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\pipe\rysh-conpty-%d-%x`, os.Getpid(), r))
	if err != nil {
		return 0, 0, err
	}
	read, err = windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_INBOUND|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 64<<10, 64<<10, 0, nil)
	if err != nil {
		return 0, 0, err
	}
	write, err = windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		closeHandles(read)
		return 0, 0, err
	}
	return read, write, nil
}

// Read reads the console's output. Once the shell has exited and the console
// is released, the pipe is broken and Read returns io.EOF.
func (c *Console) Read(p []byte) (int, error) {
	n, err := c.out.Read(p)
	return eofOnBrokenPipe(n, err, isBrokenPipe)
}

func isBrokenPipe(err error) bool { return errors.Is(err, windows.ERROR_BROKEN_PIPE) }

// releaseConsole closes the pseudo-console. hpc is zeroed under mu first, so a
// Resize never reaches a closed HPCON; ClosePseudoConsole itself runs outside
// mu because it can block until the output pipe is drained (or closed).
func (c *Console) releaseConsole() {
	c.mu.Lock()
	hpc := c.hpc
	c.hpc = 0
	c.mu.Unlock()
	if hpc != 0 {
		windows.ClosePseudoConsole(hpc)
	}
}

// SetReadDeadline sets the deadline for Read, as on an *os.File.
func (c *Console) SetReadDeadline(t time.Time) error { return c.out.SetReadDeadline(t) }

// Write writes keystrokes to the console's input.
func (c *Console) Write(p []byte) (int, error) { return c.in.Write(p) }

// Resize changes the console size; the shell sees it as a window resize.
func (c *Console) Resize(s Size) error {
	if _, err := NewSize(int(s.Rows), int(s.Cols)); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hpc == 0 {
		return errors.New("conpty: console released (shell exited or console closed)")
	}
	return windows.ResizePseudoConsole(c.hpc, coord(s))
}

// Pid is the shell's process id.
func (c *Console) Pid() int { return c.pid }

// Wait blocks until the shell exits and returns its exit code. It is safe to
// call from any goroutine, any number of times, before or after Close.
func (c *Console) Wait() (uint32, error) {
	<-c.done
	return c.exitCode, c.exitErr
}

// Kill ends every process in the console's job — the shell and everything it
// started. It is what `[proxy] strict` needs in place of kill(-pgid).
func (c *Console) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return errors.New("conpty: no job (console closed)")
	}
	return windows.TerminateJobObject(c.job, 1)
}

// Close tears the console down. The output pipe is closed first, which also
// cancels a Read parked on it, because ClosePseudoConsole can otherwise block
// until undrained output is read (that also unblocks a release watchExit has
// already started, which this one then waits for); closing the job then ends
// every process still in it, and reap releases the shell's handle.
func (c *Console) Close() error {
	c.closeOnce.Do(func() {
		if c.in != nil {
			_ = c.in.Close()
		}
		if c.out != nil {
			_ = c.out.Close()
		}
		c.hpcRelease.do()
		c.mu.Lock()
		closeHandles(c.job)
		c.job = 0
		c.mu.Unlock()
	})
	return nil
}

func closeHandles(hs ...windows.Handle) {
	for _, h := range hs {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}
