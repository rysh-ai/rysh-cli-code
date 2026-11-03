// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SignalSend is one "send" JSON-RPC request the fake daemon answered.
type SignalSend struct {
	Recipient string // the 1:1 recipient, or "" for a group send
	GroupID   string
	Message   string
}

// Signal is a fake signal-cli JSON-RPC daemon (`signal-cli daemon --socket`)
// on a UNIX socket: newline-delimited JSON-RPC, "send" answered with a
// timestamp, and "receive" notifications pushed by PushDM / PushGroup.
type Signal struct {
	Account string // the linked number the adapter is configured with

	dir string
	ln  net.Listener

	mu     sync.Mutex
	conn   net.Conn
	ready  chan struct{}
	sends  []SignalSend
	stamp  int64
	closed bool
}

// NewSignal starts a fake daemon for account on a socket in a fresh temp dir.
func NewSignal(account string) *Signal {
	dir, err := os.MkdirTemp("", "fakesignal")
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: signal temp dir: %v", err))
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "sig.sock"))
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: signal listen: %v", err))
	}
	f := &Signal{Account: account, dir: dir, ln: ln, ready: make(chan struct{}), stamp: 1720000000000}
	go f.accept()
	return f
}

// Addr is the socket path to put in ChannelConfig.SidecarAddr.
func (f *Signal) Addr() string { return f.ln.Addr().String() }

// Close stops the daemon and removes its socket.
func (f *Signal) Close() {
	f.mu.Lock()
	f.closed = true
	if f.conn != nil {
		_ = f.conn.Close()
	}
	f.mu.Unlock()
	_ = f.ln.Close()
	_ = os.RemoveAll(f.dir)
}

// Sends returns a copy of every send so far.
func (f *Signal) Sends() []SignalSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SignalSend(nil), f.sends...)
}

// PushDM pushes a 1:1 message from number to the linked account.
func (f *Signal) PushDM(from, text string) { f.push(from, text, "", nil) }

// PushGroup pushes a group message; mentions are the numbers @-mentioned.
func (f *Signal) PushGroup(groupID, from, text string, mentions ...string) {
	f.push(from, text, groupID, mentions)
}

func (f *Signal) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.conn == nil {
			close(f.ready)
		} else {
			_ = f.conn.Close() // a reconnect replaces the old connection
		}
		f.conn = c
		f.mu.Unlock()
		go f.serve(c)
	}
}

func (f *Signal) serve(c net.Conn) {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			ID     any            `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		id, _ := json.Marshal(req.ID)
		f.mu.Lock()
		f.stamp++
		stamp := f.stamp
		if req.Method == "send" {
			s := SignalSend{}
			s.Message, _ = req.Params["message"].(string)
			s.GroupID, _ = req.Params["groupId"].(string)
			if rec, ok := req.Params["recipient"].([]any); ok && len(rec) > 0 {
				s.Recipient, _ = rec[0].(string)
			}
			f.sends = append(f.sends, s)
		}
		f.mu.Unlock()
		f.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"timestamp":%d}}`, id, stamp))
	}
}

func (f *Signal) push(from, text, groupID string, mentions []string) {
	select {
	case <-f.ready:
	case <-time.After(5 * time.Second):
		panic("fakeplatform: signal adapter never connected")
	}
	type mention struct {
		Number string `json:"number"`
	}
	data := map[string]any{"message": text}
	if groupID != "" {
		data["groupInfo"] = map[string]string{"groupId": groupID}
	}
	if len(mentions) > 0 {
		ms := make([]mention, len(mentions))
		for i, m := range mentions {
			ms[i] = mention{Number: m}
		}
		data["mentions"] = ms
	}
	f.mu.Lock()
	f.stamp++
	stamp := f.stamp
	f.mu.Unlock()
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "receive",
		"params": map[string]any{
			"account": f.Account,
			"envelope": map[string]any{
				"source": from, "sourceName": "u" + from, "timestamp": stamp, "dataMessage": data,
			},
		},
	})
	f.write(string(frame))
}

func (f *Signal) write(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == nil || f.closed {
		return
	}
	_, _ = f.conn.Write([]byte(line + "\n"))
}
