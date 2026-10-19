// SPDX-License-Identifier: Apache-2.0

// Package peer exchanges explicit messages with a server workspace. It has no
// NATS upstream, sharing, pane-control or agent-execution dependencies.
package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type Connection struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url"`
	Workspace string `json:"workspace"`
	Board     string `json:"board"`
	KeySecret string `json:"key_secret"`
	SessionID string `json:"session_id"`
}

type Message struct {
	WorkspaceID string    `json:"workspace_id"`
	Board       string    `json:"board"`
	Sequence    int64     `json:"sequence"`
	ID          string    `json:"id"`
	ThreadID    string    `json:"thread_id"`
	UserID      string    `json:"user_id"`
	SessionID   string    `json:"session_id"`
	SessionName string    `json:"session_name"`
	AgentID     string    `json:"agent_id"`
	AgentName   string    `json:"agent_name"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"created_at"`
}

type Post struct {
	ID          string `json:"id"`
	ThreadID    string `json:"thread_id,omitempty"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	Text        string `json:"text"`
}

type Page struct {
	Messages []Message `json:"messages"`
	Next     int64     `json:"next"`
	HasMore  bool      `json:"has_more"`
}

type Receipt struct {
	Message  Message `json:"message"`
	Replayed bool    `json:"replayed"`
}
type Status struct {
	Workspace string `json:"workspace_id"`
	Board     string `json:"board"`
	User      string `json:"user_id"`
	Role      string `json:"role"`
	Transport string `json:"transport"`
}

var boardName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidBoard(name string) bool { return boardName.MatchString(name) }

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("peer URL must be a server origin without credentials, query or path")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return fmt.Errorf("peer connections require HTTPS (HTTP is allowed only on loopback)")
	}
	return nil
}

func connectionPath(root, session string) string {
	return filepath.Join(root, "peers", fmt.Sprintf("%x.json", sha256.Sum256([]byte(session))))
}

func Load(root, session string) (Connection, error) {
	var c Connection
	data, err := os.ReadFile(connectionPath(root, session))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

// Save is atomic and stores only a secret's name, never its value.
func Save(root, session string, c Connection) error {
	path := connectionPath(root, session)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".peer-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

type Client struct {
	Connection Connection
	Key        string
}

// Never follow redirects with workspace credentials, including same-host ones.
func (c Client) request(ctx context.Context, method, path string, body, result any) error {
	if err := ValidateURL(c.Connection.URL); err != nil {
		return err
	}
	if c.Key == "" {
		return fmt.Errorf("peer API key secret is unavailable")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Connection.URL, "/")+path, reader)
	if err != nil {
		return fmt.Errorf("invalid peer request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("peer server unreachable; no delivery confirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not reflect arbitrary server bodies (or credentials) into a pane.
		return fmt.Errorf("peer server returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result); err != nil {
		return fmt.Errorf("peer server returned invalid JSON")
	}
	return nil
}

func (c Client) ResolveWorkspace(ctx context.Context) (string, error) {
	var info struct {
		Workspace string `json:"workspace_id"`
	}
	err := c.request(ctx, http.MethodGet, "/api/server-info", nil, &info)
	if err != nil {
		return "", err
	}
	if id, err := uuid.Parse(info.Workspace); err != nil || id == uuid.Nil {
		return "", fmt.Errorf("API key did not resolve to a workspace")
	}
	return info.Workspace, nil
}

func (c Client) path() (string, error) {
	if _, err := uuid.Parse(c.Connection.Workspace); err != nil || !ValidBoard(c.Connection.Board) {
		return "", fmt.Errorf("invalid peer workspace or board")
	}
	return "/api/workspaces/" + c.Connection.Workspace + "/peer-boards/" + c.Connection.Board, nil
}
func (c Client) Status(ctx context.Context) (Status, error) {
	var s Status
	path, err := c.path()
	if err != nil {
		return s, err
	}
	err = c.request(ctx, http.MethodGet, path, nil, &s)
	if err == nil && (s.Workspace != c.Connection.Workspace || s.Board != c.Connection.Board || s.Transport != "messages-only") {
		err = fmt.Errorf("server does not support this peer board")
	}
	return s, err
}
func (c Client) Send(ctx context.Context, p Post) (Receipt, error) {
	var r Receipt
	path, err := c.path()
	if err != nil {
		return r, err
	}
	err = c.request(ctx, http.MethodPost, path+"/messages", p, &r)
	if err == nil && (r.Message.ID != p.ID || r.Message.Sequence < 1 || r.Message.WorkspaceID != c.Connection.Workspace || r.Message.Board != c.Connection.Board) {
		err = fmt.Errorf("server did not confirm this message")
	}
	return r, err
}
func (c Client) Tail(ctx context.Context, after *int64, limit int) (Page, error) {
	var p Page
	path, err := c.path()
	if err != nil {
		return p, err
	}
	query := fmt.Sprintf("?limit=%d", limit)
	if after != nil {
		query += fmt.Sprintf("&after=%d", *after)
	}
	err = c.request(ctx, http.MethodGet, path+"/messages"+query, nil, &p)
	return p, err
}

// Remote text is data. Strip terminal controls before rendering to a terminal.
func Display(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}
func FormatMessage(m Message) string {
	kind := "post"
	if m.ID != m.ThreadID {
		kind = "reply"
	}
	return fmt.Sprintf("[%d] %s · %s/%s · user %s · session %s\n  %s id=%s thread=%s\n%s\n", m.Sequence, m.CreatedAt.UTC().Format(time.RFC3339), Display(m.SessionName), Display(m.AgentName), Display(m.UserID), Display(m.SessionID), kind, Display(m.ID), Display(m.ThreadID), Display(m.Text))
}
