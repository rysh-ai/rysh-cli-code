package tools

import (
	"bytes"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// BackgroundSession tracks a single background shell process.
type BackgroundSession struct {
	ID        string
	Command   string
	WorkDir   string
	StartedAt time.Time
	Cmd       *exec.Cmd
	buf       *ringBuffer
	done      chan struct{}
	exitCode  int
	exitErr   error
	finished  bool
	mu        sync.Mutex
}

// BackgroundSessionManager manages background shell sessions.
type BackgroundSessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*BackgroundSession
}

// NewBackgroundSessionManager creates a new manager.
func NewBackgroundSessionManager() *BackgroundSessionManager {
	return &BackgroundSessionManager{
		sessions: make(map[string]*BackgroundSession),
	}
}

// Start launches a background bash command and returns a session ID.
func (m *BackgroundSessionManager) Start(id, command, workDir string) error {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = workDir

	rb := newRingBuffer(256 * 1024) // 256KB ring buffer
	cmd.Stdout = rb
	cmd.Stderr = rb

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start: %w", err)
	}

	sess := &BackgroundSession{
		ID:        id,
		Command:   command,
		WorkDir:   workDir,
		StartedAt: time.Now(),
		Cmd:       cmd,
		buf:       rb,
		done:      make(chan struct{}),
	}

	// Wait for the process in a goroutine.
	go func() {
		err := cmd.Wait()
		sess.mu.Lock()
		sess.finished = true
		sess.exitErr = err
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				sess.exitCode = exitErr.ExitCode()
			} else {
				sess.exitCode = -1
			}
		}
		sess.mu.Unlock()
		close(sess.done)
	}()

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	return nil
}

// Get retrieves a session by ID.
func (m *BackgroundSessionManager) Get(id string) (*BackgroundSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Kill terminates a background session.
func (m *BackgroundSessionManager) Kill(id string) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	if sess.Cmd.Process == nil {
		return fmt.Errorf("session %s has no process", id)
	}
	return sess.Cmd.Process.Kill()
}

// Remove removes a finished session from the manager.
func (m *BackgroundSessionManager) Remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// List returns info about all sessions.
func (m *BackgroundSessionManager) List() []BackgroundSessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	infos := make([]BackgroundSessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		info := BackgroundSessionInfo{
			ID:        s.ID,
			Command:   s.Command,
			StartedAt: s.StartedAt,
			Finished:  s.finished,
			ExitCode:  s.exitCode,
			PID:       0,
		}
		if s.Cmd.Process != nil {
			info.PID = s.Cmd.Process.Pid
		}
		s.mu.Unlock()
		infos = append(infos, info)
	}
	return infos
}

// ReadOutput returns the current output from the session's ring buffer.
func (m *BackgroundSessionManager) ReadOutput(id string, tailBytes int) (string, bool, error) {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return "", false, fmt.Errorf("session %s not found", id)
	}

	output := sess.buf.String()
	if tailBytes > 0 && len(output) > tailBytes {
		output = output[len(output)-tailBytes:]
	}

	sess.mu.Lock()
	finished := sess.finished
	sess.mu.Unlock()

	return output, finished, nil
}

// Cleanup kills all running sessions and clears the map.
func (m *BackgroundSessionManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.finished && s.Cmd.Process != nil {
			_ = s.Cmd.Process.Kill()
		}
		s.mu.Unlock()
	}
	m.sessions = make(map[string]*BackgroundSession)
}

// BackgroundSessionInfo is a serializable view of a background session.
type BackgroundSessionInfo struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	StartedAt time.Time `json:"started_at"`
	Finished  bool      `json:"finished"`
	ExitCode  int       `json:"exit_code"`
	PID       int       `json:"pid"`
}

// ringBuffer is a fixed-size circular buffer that implements io.Writer.
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	size int
	pos  int
	full bool
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		data: make([]byte, size),
		size: size,
	}
}

func (rb *ringBuffer) Write(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	n := len(p)
	if n >= rb.size {
		// Data larger than buffer — keep only the tail.
		copy(rb.data, p[n-rb.size:])
		rb.pos = 0
		rb.full = true
		return n, nil
	}

	// Write into the ring.
	remaining := rb.size - rb.pos
	if n <= remaining {
		copy(rb.data[rb.pos:], p)
	} else {
		copy(rb.data[rb.pos:], p[:remaining])
		copy(rb.data, p[remaining:])
	}

	rb.pos = (rb.pos + n) % rb.size
	if !rb.full && rb.pos < n {
		rb.full = true
	}

	return n, nil
}

func (rb *ringBuffer) String() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if !rb.full {
		return string(rb.data[:rb.pos])
	}

	var buf bytes.Buffer
	buf.Write(rb.data[rb.pos:])
	buf.Write(rb.data[:rb.pos])
	return buf.String()
}
