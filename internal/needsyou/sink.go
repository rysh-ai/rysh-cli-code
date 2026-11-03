// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Alert is what a sink is told. Every field is a name, an id, a reason word or
// a time: nothing from a pane's screen.
type Alert struct {
	// Event is EventNeedsYou (also when empty) or EventDone.
	Event    string
	Session  string
	Host     string
	PaneID   string
	PaneName string
	Reason   string
	Since    int64 // unix millis
	// Ran is how long a finished run took (EventDone only).
	Ran time.Duration
}

// event is a.Event, with the needs-you default.
func (a Alert) event() string {
	if a.Event == "" {
		return EventNeedsYou
	}
	return a.Event
}

// Text is the one-line, human form of the alert (a push notification's body).
func (a Alert) Text() string {
	var s string
	if a.event() == EventDone {
		verb := "finished"
		if a.Reason == ReasonFailed {
			verb = "failed"
		}
		s = "rysh: " + a.PaneName + " " + verb + " after " + a.Ran.Round(time.Second).String()
	} else {
		s = "rysh: " + a.PaneName + " needs you"
		if a.Reason != "" {
			s += " (" + a.Reason + ")"
		}
	}
	s += " — session " + a.Session
	if a.Host != "" {
		s += " on " + a.Host
	}
	return s
}

// Sink delivers an alert somewhere outside the session.
type Sink interface {
	Name() string
	Send(ctx context.Context, a Alert) error
}

// webhookTimeout bounds one POST.
const webhookTimeout = 10 * time.Second

// WebhookSink POSTs a fixed JSON document to a URL. The URL often carries a
// secret (an ntfy topic, a chat webhook token), so it is never put in an error
// or a log line.
type WebhookSink struct {
	url    string
	client *http.Client
}

// NewWebhookSink returns a sink for url. Redirects are not followed: the
// document goes where it was configured to go, or nowhere.
func NewWebhookSink(url string) *WebhookSink {
	return &WebhookSink{url: url, client: &http.Client{
		Timeout: webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Name implements Sink.
func (s *WebhookSink) Name() string { return "webhook" }

// webhookDoc is the wire document. Add fields, never rename.
type webhookDoc struct {
	Event    string `json:"event"`
	Session  string `json:"session"`
	Host     string `json:"host"`
	Pane     string `json:"pane"`
	PaneName string `json:"pane_name"`
	Reason   string `json:"reason"`
	Since    int64  `json:"since"`
	Text     string `json:"text"`
	// RanSeconds is set on a "done" event only.
	RanSeconds int64 `json:"ran_seconds,omitempty"`
}

// Send implements Sink.
func (s *WebhookSink) Send(ctx context.Context, a Alert) error {
	body, err := json.Marshal(webhookDoc{
		Event: a.event(), Session: a.Session, Host: a.Host, Pane: a.PaneID,
		PaneName: a.PaneName, Reason: a.Reason, Since: a.Since, Text: a.Text(),
		RanSeconds: int64(a.Ran / time.Second),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("webhook: invalid URL") // the parse error would quote it
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("webhook: request failed") // *url.Error quotes the URL
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

// CommandSink runs a program, without a shell, with the alert in RYSH_NOTIFY_*
// environment variables. It runs with the daemon's own environment and user,
// the same trust as every other command rysh.config.yaml configures.
type CommandSink struct{ argv []string }

// NewCommandSink returns a sink for argv (argv[0] is the program).
func NewCommandSink(argv []string) *CommandSink {
	return &CommandSink{argv: append([]string(nil), argv...)}
}

// Name implements Sink.
func (s *CommandSink) Name() string { return "command" }

// Send implements Sink. Output is discarded; a non-zero exit is an error.
func (s *CommandSink) Send(ctx context.Context, a Alert) error {
	if len(s.argv) == 0 {
		return errors.New("command: empty")
	}
	cmd := exec.CommandContext(ctx, s.argv[0], s.argv[1:]...)
	cmd.Env = append(os.Environ(),
		"RYSH_NOTIFY_EVENT="+a.event(),
		"RYSH_NOTIFY_SESSION="+a.Session,
		"RYSH_NOTIFY_HOST="+a.Host,
		"RYSH_NOTIFY_PANE="+a.PaneID,
		"RYSH_NOTIFY_PANE_NAME="+a.PaneName,
		"RYSH_NOTIFY_REASON="+a.Reason,
		"RYSH_NOTIFY_SINCE="+strconv.FormatInt(a.Since, 10),
		"RYSH_NOTIFY_TEXT="+a.Text(),
		"RYSH_NOTIFY_RAN_SECONDS="+strconv.FormatInt(int64(a.Ran/time.Second), 10),
	)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command: %w", err)
	}
	return nil
}
