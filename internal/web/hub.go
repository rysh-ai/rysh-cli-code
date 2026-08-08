package web

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Hub maintains the set of active WebSocket clients and broadcasts messages
// to all of them. Modelled after the Gorilla WebSocket chat example.
type Hub struct {
	clients     map[*wsClient]bool
	broadcast   chan hubMessage
	register    chan *wsClient
	unregister  chan *wsClient
	streamCount atomic.Int32 // number of registered ?stream=1 (content-plane) clients
	// plainCount is the number of registered clients that did NOT opt into the
	// content plane. They are the only consumers of the blind full-snapshot
	// poll, which is expensive enough (a whole workspace's pane content) that
	// pollSnapshots checks this before building one at all.
	plainCount atomic.Int32
	// onCount, when set (before run starts), is invoked with the new total
	// client count on every register/unregister transition. Runs on the hub
	// goroutine — keep it cheap (the session-record presence update is a
	// small file write on rare connect/disconnect events).
	onCount func(n int)
}

// notifyCount reports the current client count to the listener, if any.
func (h *Hub) notifyCount() {
	if h.onCount != nil {
		h.onCount(len(h.clients))
	}
}

// hubMessage is a broadcast payload with an optional recipient predicate. When
// to == nil it goes to every client; otherwise only to clients for which
// to(client) returns true (used to route layout-only/content-stream messages to
// opted-in clients and full snapshots to the rest).
type hubMessage struct {
	data []byte
	to   func(*wsClient) bool
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*wsClient]bool),
		broadcast:  make(chan hubMessage, 64),
		register:   make(chan *wsClient),
		unregister: make(chan *wsClient),
	}
}

// sendAll broadcasts data to every connected client.
func (h *Hub) sendAll(data []byte) { h.broadcast <- hubMessage{data: data} }

// sendWhere broadcasts data only to clients for which to(client) is true.
func (h *Hub) sendWhere(data []byte, to func(*wsClient) bool) {
	h.broadcast <- hubMessage{data: data, to: to}
}

// hasStream reports whether any client opted into the content plane.
func (h *Hub) hasStream() bool { return h.streamCount.Load() > 0 }

// hasPlain reports whether any client still needs the blind full-snapshot poll
// (i.e. did NOT opt into the content plane).
func (h *Hub) hasPlain() bool { return h.plainCount.Load() > 0 }

// countClient adjusts the per-kind client counters by delta. Every add/remove
// of a client goes through here so the two counters cannot drift apart.
func (h *Hub) countClient(c *wsClient, delta int32) {
	if c.streamContent {
		h.streamCount.Add(delta)
		return
	}
	h.plainCount.Add(delta)
}

func (h *Hub) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
			}
			return
		case client := <-h.register:
			h.clients[client] = true
			h.countClient(client, 1)
			h.notifyCount()
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				h.countClient(client, -1)
				h.notifyCount()
			}
		case message := <-h.broadcast:
			dropped := false
			for client := range h.clients {
				if message.to != nil && !message.to(client) {
					continue
				}
				select {
				case client.send <- message.data:
				default:
					// Client too slow — disconnect.
					delete(h.clients, client)
					close(client.send)
					h.countClient(client, -1)
					dropped = true
				}
			}
			if dropped {
				h.notifyCount()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// WebSocket client
// ---------------------------------------------------------------------------

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

// wsClient is a middleman between the WebSocket connection and the hub.
type wsClient struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	// streamContent: the client opted into the content plane — it receives
	// layout-only snapshots (event-driven) plus per-pane content deltas, instead
	// of the heavy full snapshot on the 200ms poll. Set from the ?stream=1 query
	// param at connect; immutable thereafter.
	streamContent bool
	// sizeClientID identifies this connection in pane size arbitration (see
	// msg.MsgPaneResize): a pane sizes its PTY to the smallest viewport showing
	// it, so two app windows on one pane must be told apart. Assigned at
	// connect; immutable thereafter.
	sizeClientID string
	// sizedPanes records which panes this connection has claimed a size on, so
	// the claims can be withdrawn when the socket closes. A pane cannot test a
	// web client's liveness the way it tests a TUI's pid, so this release is
	// the ONLY thing that stops a closed window from clamping a pane forever.
	// Written from the read pump and read on disconnect.
	sizedPanes map[string]bool
}

// writePump pumps messages from the hub to the WebSocket connection.
func (c *wsClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump pumps messages from the WebSocket connection to the server.
func (c *wsClient) readPump(server *Server) {
	defer func() {
		// Withdraw this window's pane size claims BEFORE unregistering. A pane
		// sizes its PTY to the smallest viewport showing it, and it cannot test
		// whether a web client is still there the way it tests a TUI's pid — so
		// without this a closed app window would clamp every pane it had sized
		// for the life of the daemon.
		server.releasePaneSizes(c)
		c.hub.unregister <- c
		c.conn.Close()
	}()

	// 64KB was too small for browser_result commands, which carry a base64
	// screenshot (~100KB+): gorilla/websocket rejects an over-limit frame and
	// closes the connection, so the reply was dropped and the browser_action
	// tool timed out. Allow large inbound command frames.
	c.conn.SetReadLimit(32 * 1024 * 1024)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		var wsMsg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(message, &wsMsg) != nil {
			continue
		}

		if wsMsg.Type == "command" {
			var cmd struct {
				Action string          `json:"action"`
				Params json.RawMessage `json:"params,omitempty"`
			}
			if json.Unmarshal(wsMsg.Data, &cmd) == nil {
				// Routed through the client-aware dispatcher so request/reply
				// exchanges (completion, W7) can answer THIS client only;
				// everything else falls through to the broadcast handler.
				server.handleClientCommand(c, cmd.Action, cmd.Params)
			}
		}
	}
}
