// SPDX-License-Identifier: Apache-2.0

package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Browser is a live CDP connection to a launched Chromium plus the process
// handle. Goroutine-safe: Call serializes writes and correlates responses by
// id, so the headless actor can execute actions from worker goroutines.
type Browser struct {
	cmd  *exec.Cmd
	conn *websocket.Conn

	writeMu sync.Mutex // one CDP frame writer at a time

	mu        sync.Mutex // guards nextID, pending, session/tab state
	nextID    int64
	pending   map[int64]chan cdpResult
	sessionID string // attached session for the current ("active") tab
	targetID  string // target backing sessionID

	// pidFileDir is the user-data-dir holding this launch's headless pidfile
	// ("" for headed launches). Close removes the pidfile so only orphaned
	// Chromiums are ever swept.
	pidFileDir string

	closed chan struct{}
}

type cdpResult struct {
	Result json.RawMessage
	Err    error
}

type cdpFrame struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// connect dials the browser-level DevTools WebSocket and starts the read loop.
func connect(ctx context.Context, wsURL string, cmd *exec.Cmd) (*Browser, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	// CDP frames (get_html on heavy pages, screenshots) can be large.
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial DevTools %s: %w", wsURL, err)
	}
	conn.SetReadLimit(64 << 20)
	b := &Browser{
		cmd:     cmd,
		conn:    conn,
		pending: map[int64]chan cdpResult{},
		closed:  make(chan struct{}),
	}
	go b.readLoop()
	return b, nil
}

func (b *Browser) readLoop() {
	defer close(b.closed)
	for {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			// Connection gone: fail all pending calls.
			b.mu.Lock()
			for id, ch := range b.pending {
				ch <- cdpResult{Err: fmt.Errorf("devtools connection closed: %w", err)}
				delete(b.pending, id)
			}
			b.mu.Unlock()
			return
		}
		var f cdpFrame
		if json.Unmarshal(data, &f) != nil || f.ID == 0 {
			continue // event or unparseable — rysh polls state, no event handling needed
		}
		b.mu.Lock()
		ch, ok := b.pending[f.ID]
		if ok {
			delete(b.pending, f.ID)
		}
		b.mu.Unlock()
		if !ok {
			continue
		}
		if f.Error != nil {
			ch <- cdpResult{Err: fmt.Errorf("cdp: %s (%d)", f.Error.Message, f.Error.Code)}
		} else {
			ch <- cdpResult{Result: f.Result}
		}
	}
}

// call sends one CDP command (optionally session-scoped) and waits for its
// response.
func (b *Browser) call(sessionID, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = data
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan cdpResult, 1)
	b.pending[id] = ch
	b.mu.Unlock()

	frame, _ := json.Marshal(cdpFrame{ID: id, Method: method, Params: raw, SessionID: sessionID})
	b.writeMu.Lock()
	err := b.conn.WriteMessage(websocket.TextMessage, frame)
	b.writeMu.Unlock()
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		return res.Result, res.Err
	case <-time.After(45 * time.Second):
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, fmt.Errorf("cdp: %s timed out", method)
	case <-b.closed:
		return nil, fmt.Errorf("cdp: connection closed")
	}
}

// Page calls a CDP method on the current tab's session.
func (b *Browser) Page(method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	sid := b.sessionID
	b.mu.Unlock()
	if sid == "" {
		return nil, fmt.Errorf("no attached tab")
	}
	return b.call(sid, method, params)
}

// targetInfo mirrors CDP Target.TargetInfo (page targets only).
type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

// pages lists page-type targets.
func (b *Browser) pages() ([]targetInfo, error) {
	res, err := b.call("", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	var pages []targetInfo
	for _, t := range out.TargetInfos {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// attach binds the Browser's "active tab" session to the given target.
func (b *Browser) attach(targetID string) error {
	res, err := b.call("", "Target.attachToTarget", map[string]any{
		"targetId": targetID, "flatten": true,
	})
	if err != nil {
		return err
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return err
	}
	b.mu.Lock()
	b.sessionID = out.SessionID
	b.targetID = targetID
	b.mu.Unlock()
	// Bring it to the front so screenshots/interaction hit the visible tab.
	_, _ = b.call("", "Target.activateTarget", map[string]any{"targetId": targetID})
	return nil
}

// attachFirstPage attaches to the first existing page target, creating one
// when the browser started without any.
func (b *Browser) attachFirstPage() error {
	pages, err := b.pages()
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		res, err := b.call("", "Target.createTarget", map[string]any{"url": "about:blank"})
		if err != nil {
			return err
		}
		var out struct {
			TargetID string `json:"targetId"`
		}
		if err := json.Unmarshal(res, &out); err != nil {
			return err
		}
		return b.attach(out.TargetID)
	}
	return b.attach(pages[0].TargetID)
}

// CurrentTargetID returns the attached tab's target id.
func (b *Browser) CurrentTargetID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.targetID
}

// Close shuts the browser down: graceful Browser.close, then a hard kill
// after a short grace period. The headless pidfile is removed so the
// orphan sweep never chases a browser that exited cleanly.
func (b *Browser) Close() {
	_, _ = b.call("", "Browser.close", nil)
	done := make(chan struct{})
	go func() {
		if b.cmd != nil {
			_, _ = b.cmd.Process.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
	}
	_ = b.conn.Close()
	if b.pidFileDir != "" {
		removeHeadlessPidFile(b.pidFileDir)
	}
}
