package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
)

// stdioMaxLine bounds a single JSON-RPC line read from the server's stdout. MCP
// messages are newline-delimited JSON with no embedded newlines; 8 MiB is
// generous headroom for large tool results.
const stdioMaxLine = 8 << 20

// stdioTransport speaks newline-delimited JSON-RPC to a server over a writer
// (its stdin) and a reader (its stdout). A single reader goroutine demultiplexes
// responses to per-request channels and forwards notifications. The process-
// spawning constructor (newStdioTransport) wraps the pipe-level constructor
// (newStdioPipe), which is also what tests drive with in-memory pipes.
type stdioTransport struct {
	name    string
	in      io.WriteCloser
	closeFn func() error

	writeMu sync.Mutex // serializes writes to in

	mu       sync.Mutex
	pending  map[string]chan *jsonrpcMessage
	notifyFn func(method string, params json.RawMessage)
	closed   bool

	done chan struct{}
}

// newStdioTransport starts the server command and begins reading its stdout. The
// process inherits env (plus extraEnv "KEY=VAL" entries) and runs in workDir.
func newStdioTransport(name, command string, args, extraEnv []string, workDir string) (*stdioTransport, error) {
	if command == "" {
		return nil, fmt.Errorf("stdio transport requires a command")
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(extraEnv)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", command, err)
	}

	closeFn := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		go func() { _ = cmd.Wait() }() // reap to avoid a zombie
		return nil
	}

	t := newStdioPipe(name, stdin, stdout, closeFn)
	go drainStderr(name, stderr)
	return t, nil
}

// newStdioPipe builds a stdio transport over an arbitrary writer/reader pair and
// starts its reader loop. closeFn is invoked once on close to release the
// underlying resources (kill the process, or close test pipes).
func newStdioPipe(name string, in io.WriteCloser, out io.Reader, closeFn func() error) *stdioTransport {
	t := &stdioTransport{
		name:    name,
		in:      in,
		closeFn: closeFn,
		pending: make(map[string]chan *jsonrpcMessage),
		done:    make(chan struct{}),
	}
	go t.readLoop(out)
	return t
}

func (t *stdioTransport) onNotification(fn func(method string, params json.RawMessage)) {
	t.mu.Lock()
	t.notifyFn = fn
	t.mu.Unlock()
}

// readLoop reads newline-delimited JSON from out until EOF or close, routing each
// message to a waiting caller or the notification handler.
func (t *stdioTransport) readLoop(out io.Reader) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64*1024), stdioMaxLine)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m jsonrpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			slog.Debug("mcp stdio: unparseable line", "server", t.name, "err", err)
			continue
		}

		// Notification (method, no id) or server-initiated request (method+id).
		if m.Method != "" {
			if len(m.ID) == 0 {
				t.mu.Lock()
				fn := t.notifyFn
				t.mu.Unlock()
				if fn != nil {
					fn(m.Method, m.Params)
				}
			} else {
				// rysh is a tools-only client; it does not service server
				// requests (sampling/roots). Ignore rather than hang.
				slog.Debug("mcp stdio: ignoring server request", "server", t.name, "method", m.Method)
			}
			continue
		}

		// Response: deliver to the pending caller keyed by id.
		key := string(m.ID)
		t.mu.Lock()
		ch, ok := t.pending[key]
		if ok {
			delete(t.pending, key)
		}
		t.mu.Unlock()
		if ok {
			mc := m
			ch <- &mc
		}
	}

	// Stream ended: fail every in-flight request so callers unblock.
	t.failAllPending(fmt.Errorf("mcp server %q closed the connection", t.name))
}

func (t *stdioTransport) failAllPending(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	pending := t.pending
	t.pending = make(map[string]chan *jsonrpcMessage)
	t.mu.Unlock()

	close(t.done)
	for _, ch := range pending {
		ch <- &jsonrpcMessage{Error: &rpcError{Code: -32000, Message: err.Error()}}
	}
}

func (t *stdioTransport) roundTrip(ctx context.Context, req jsonrpcRequest) (*jsonrpcMessage, error) {
	key := string(req.ID)
	ch := make(chan *jsonrpcMessage, 1)

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("mcp server %q is not connected", t.name)
	}
	t.pending[key] = ch
	t.mu.Unlock()

	if err := t.writeMessage(req); err != nil {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return nil, ctx.Err()
	case <-t.done:
		return nil, fmt.Errorf("mcp server %q disconnected", t.name)
	case m := <-ch:
		if m.Error != nil {
			return m, m.Error
		}
		return m, nil
	}
}

func (t *stdioTransport) notify(_ context.Context, req jsonrpcRequest) error {
	return t.writeMessage(req)
}

func (t *stdioTransport) writeMessage(req jsonrpcRequest) error {
	req.JSONRPC = "2.0"
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.in.Write(data); err != nil {
		return fmt.Errorf("write to %q: %w", t.name, err)
	}
	return nil
}

func (t *stdioTransport) close() error {
	t.failAllPending(fmt.Errorf("mcp server %q closed", t.name))
	if t.closeFn != nil {
		return t.closeFn()
	}
	return nil
}

// drainStderr forwards the child's stderr to the rysh debug log so server logs
// are observable without polluting the agent's tool output.
func drainStderr(name string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 16*1024), 1<<20)
	for scanner.Scan() {
		slog.Debug("mcp server stderr", "server", name, "line", scanner.Text())
	}
}
