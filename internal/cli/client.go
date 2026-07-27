// Package cli implements CLI subcommands that communicate with a running
// rysh session via NATS.
package cli

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Client connects to a running rysh session's NATS server.
type Client struct {
	nc      *nats.Conn
	pub     *msg.NATSPublisher
	codecs  *msg.CodecRegistry
	session string
}

// NewClient connects to the NATS server at the given port.
func NewClient(port int, sessionName string) (*Client, error) {
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	nc, err := nats.Connect(url,
		nats.Timeout(3*time.Second),
		nats.MaxReconnects(0),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to session NATS at %s: %w", url, err)
	}
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	return &Client{nc: nc, pub: pub, codecs: codecs, session: sessionName}, nil
}

// Close drains and closes the NATS connection.
func (c *Client) Close() {
	if c.nc != nil {
		_ = c.nc.Drain()
	}
}

// wsInbox returns the workspace inbox subject for the session.
func (c *Client) wsInbox() string {
	return msg.T("ws", "inbox")
}

// snapshotSubject returns the workspace snapshot subject for the session.
func (c *Client) snapshotSubject() string {
	return msg.T("ws", "snapshot")
}

// Send publishes a fire-and-forget message to the workspace inbox.
// It flushes the connection so the message is delivered before the
// short-lived CLI process exits (Close drains asynchronously).
func (c *Client) Send(message interface{}) error {
	if err := c.pub.Send(c.wsInbox(), message); err != nil {
		return err
	}
	return c.pub.Flush()
}

// Request sends a request/reply message to the workspace inbox.
func (c *Client) Request(message interface{}, timeout time.Duration) (interface{}, error) {
	return c.pub.Request(c.wsInbox(), message, timeout)
}

// RequestSnapshot requests the full workspace snapshot.
func (c *Client) RequestSnapshot(timeout time.Duration) (interface{}, error) {
	return c.pub.Request(c.snapshotSubject(), &msg.MsgGetWorkspaceSnapshot{}, timeout)
}

// SendToSubject publishes a fire-and-forget message to a specific subject.
// It flushes so the message is delivered before the CLI process exits.
func (c *Client) SendToSubject(subject string, message interface{}) error {
	if err := c.pub.Send(subject, message); err != nil {
		return err
	}
	return c.pub.Flush()
}

// RequestFromSubject sends a request/reply to a specific subject.
func (c *Client) RequestFromSubject(subject string, message interface{}, timeout time.Duration) (interface{}, error) {
	return c.pub.Request(subject, message, timeout)
}
