// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Public drive API (boat gaps #5/#6): read a run's panes, prompt one,
// interrupt it, and follow the run's events. The server side is rysh-server
// hosted_runs_public.go, dark behind RYSH_HOSTED_API_PUBLIC.

var paneIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Pane is one pane of a run's session.
type Pane struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Program   string `json:"program,omitempty"`
	NeedsYou  bool   `json:"needs_you"`
	Attention string `json:"attention,omitempty"`
}

// PromptResult is one turn's outcome (`rysh prompt --json` in the run).
type PromptResult struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Detail   string `json:"detail,omitempty"`
	Output   string `json:"output,omitempty"`
}

// Event is one server-sent event of a run's stream.
type Event struct {
	Name string
	Data json.RawMessage // the event's JSON (as text when it is not JSON)
	ID   string
}

func (c *Client) panePath(runID, suffix string) (string, error) {
	p, err := c.runPath(runID)
	if err != nil {
		return "", err
	}
	return p + "/" + suffix, nil
}

// Panes lists the run's panes and their "needs you" state.
func (c *Client) Panes(ctx context.Context, runID string) ([]Pane, error) {
	p, err := c.panePath(runID, "panes")
	if err != nil {
		return nil, err
	}
	var out struct {
		Panes []Pane `json:"panes"`
	}
	err = c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out.Panes, err
}

// Prompt sends text to a pane and waits up to waitSeconds (at most 50) for
// the turn. A turn still running then comes back with Status "timeout".
func (c *Client) Prompt(ctx context.Context, runID, pane, text string, waitSeconds int) (PromptResult, error) {
	if !paneIDRe.MatchString(pane) {
		return PromptResult{}, fmt.Errorf("%q is not a pane id", pane)
	}
	p, err := c.panePath(runID, "prompt")
	if err != nil {
		return PromptResult{}, err
	}
	var res PromptResult
	err = c.do(ctx, http.MethodPost, p, map[string]any{"pane": pane, "text": text, "wait_seconds": waitSeconds}, nil, &res)
	return res, err
}

// Interrupt ends the pane's current turn (ESC); the agent keeps its context.
func (c *Client) Interrupt(ctx context.Context, runID, pane string) error {
	if !paneIDRe.MatchString(pane) {
		return fmt.Errorf("%q is not a pane id", pane)
	}
	p, err := c.panePath(runID, "interrupt")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, p, map[string]any{"pane": pane}, nil, nil)
}

// Events follows the run's event stream, calling fn for each event until the
// server closes the stream (after "end" or "reconnect"), ctx ends, or fn
// returns an error (which Events returns). A clean close is nil.
func (c *Client) Events(ctx context.Context, runID string, fn func(Event) error) error {
	if err := c.Conn.Validate(); err != nil {
		return err
	}
	p, err := c.panePath(runID, "events")
	if err != nil {
		return err
	}
	return c.streamEvents(ctx, p, fn)
}

// streamEvents follows one server-sent event stream at path.
func (c *Client) streamEvents(ctx context.Context, p string, fn func(Event) error) error {
	if err := c.Conn.Validate(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "text/event-stream")
	// A stream has no overall deadline: ctx ends it.
	hc := &http.Client{Transport: c.http().Transport}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("could not reach the server: %s", scrub(err.Error(), []string{c.APIKey}))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return friendly(resp.StatusCode, scrub(e.Error, []string{c.APIKey}))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ev Event
	var data []string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if ev.Name != "" || len(data) > 0 {
				if ev.Name == "" {
					ev.Name = "message"
				}
				raw := strings.Join(data, "\n")
				if json.Valid([]byte(raw)) {
					ev.Data = json.RawMessage(raw)
				} else {
					b, _ := json.Marshal(raw)
					ev.Data = b
				}
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev, data = Event{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Name = value
		case "data":
			data = append(data, value)
		case "id":
			ev.ID = value
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		return errors.New("the event stream broke off")
	}
	return ctx.Err()
}
