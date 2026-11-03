// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Auth holds the bearer tokens a Client sends. It is shared between the REST
// client and the socket so a refresh done by one is used by the other.
//
// A token read from an environment variable (`--token-env`) has no refresh
// token: a 401 is then final.
type Auth struct {
	mu      sync.Mutex
	access  string
	refresh string

	// OnRefresh, when set, is called with the new tokens after a successful
	// refresh — the CLI persists them to its token store here. An error from
	// it is returned to the caller: a refresh that cannot be saved would be
	// lost on the next run, and the old refresh token is already spent.
	OnRefresh func(Tokens) error
}

// NewAuth returns an Auth holding the given tokens. refresh may be "".
func NewAuth(access, refresh string) *Auth {
	return &Auth{access: access, refresh: refresh}
}

// Access returns the current access token.
func (a *Auth) Access() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.access
}

func (a *Auth) canRefresh() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refresh != ""
}

// Client talks to one rysh-server's /api/family routes.
type Client struct {
	BaseURL string // e.g. https://dev.rysh.ai (no trailing slash needed)
	HTTP    *http.Client
	Auth    *Auth
}

// New returns a Client for baseURL with a 30s request timeout.
func New(baseURL string, auth *Auth) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Auth:    auth,
	}
}

func (c *Client) base() string { return strings.TrimRight(c.BaseURL, "/") }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do sends one request. On a 401 it refreshes the tokens (when it can) and
// retries exactly once.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	used := c.Auth.Access()
	status, err := c.doOnce(ctx, method, path, used, in, out)
	if status != http.StatusUnauthorized || !c.Auth.canRefresh() {
		return err
	}
	if rerr := c.refreshIfStale(ctx, used); rerr != nil {
		return fmt.Errorf("%w (token refresh failed: %v)", err, rerr)
	}
	_, err = c.doOnce(ctx, method, path, c.Auth.Access(), in, out)
	return err
}

func (c *Client) doOnce(ctx context.Context, method, path, token string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach %s: %w", c.base(), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, decodeAPIError(resp.StatusCode, data)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(data)) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: unexpected response (is %s a rysh server?): %w", method, path, c.base(), err)
	}
	return resp.StatusCode, nil
}

func decodeAPIError(status int, data []byte) error {
	e := &APIError{Status: status}
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Code, e.Message = body.Error, body.Message
	}
	if e.Code == "" {
		e.Code = http.StatusText(status)
	}
	return e
}

// refreshIfStale refreshes the tokens unless another caller already did so
// since `used` was read (two concurrent 401s refresh once, not twice — the
// server rotates refresh tokens, so a second refresh with the spent token
// would fail).
func (c *Client) refreshIfStale(ctx context.Context, used string) error {
	a := c.Auth
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.access != used {
		return nil
	}
	if a.refresh == "" {
		return errors.New("no refresh token")
	}
	var t Tokens
	status, err := c.doOnce(ctx, http.MethodPost, "/api/auth/refresh", "", map[string]string{"refresh_token": a.refresh}, &t)
	if err != nil {
		return err
	}
	if t.AccessToken == "" {
		return fmt.Errorf("refresh answered HTTP %d without an access token", status)
	}
	a.access = t.AccessToken
	if t.RefreshToken != "" {
		a.refresh = t.RefreshToken
	} else {
		t.RefreshToken = a.refresh
	}
	if a.OnRefresh != nil {
		if err := a.OnRefresh(t); err != nil {
			return fmt.Errorf("saving refreshed tokens: %w", err)
		}
	}
	return nil
}

// Refresh forces a token refresh (the socket calls it when its handshake is
// refused with 401).
func (c *Client) Refresh(ctx context.Context, used string) error {
	if !c.Auth.canRefresh() {
		return errors.New("no refresh token (a --token-env token cannot be refreshed)")
	}
	return c.refreshIfStale(ctx, used)
}

// Login exchanges an email and password for tokens (POST /api/auth/login).
// It does not touch c.Auth.
func (c *Client) Login(ctx context.Context, email, password string) (Tokens, error) {
	var t Tokens
	_, err := c.doOnce(ctx, http.MethodPost, "/api/auth/login", "", map[string]string{"email": email, "password": password}, &t)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return t, errors.New("login refused: wrong email or password")
		}
		return t, err
	}
	if t.AccessToken == "" {
		return t, errors.New("login answered without an access token")
	}
	return t, nil
}

// Families lists the signed-in user's families (GET /api/family/families).
func (c *Client) Families(ctx context.Context) ([]Family, error) {
	var out []Family
	err := c.do(ctx, http.MethodGet, "/api/family/families", nil, &out)
	return out, err
}

// Family fetches one family with its avatars.
func (c *Client) Family(ctx context.Context, fid string) (Family, error) {
	var out Family
	err := c.do(ctx, http.MethodGet, "/api/family/families/"+url.PathEscape(fid), nil, &out)
	return out, err
}

// Tasks lists a family's tasks.
func (c *Client) Tasks(ctx context.Context, fid string) ([]Task, error) {
	var out []Task
	err := c.do(ctx, http.MethodGet, "/api/family/families/"+url.PathEscape(fid)+"/tasks", nil, &out)
	return out, err
}

// Messages fetches an avatar's chat history, oldest first. before may be ""
// and limit 0 (server default).
func (c *Client) Messages(ctx context.Context, fid, aid, before string, limit int) ([]Message, error) {
	q := url.Values{}
	if before != "" {
		q.Set("before", before)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	p := "/api/family/families/" + url.PathEscape(fid) + "/avatars/" + url.PathEscape(aid) + "/messages"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var out struct {
		Messages []Message `json:"messages"`
	}
	err := c.do(ctx, http.MethodGet, p, nil, &out)
	return out.Messages, err
}

// SendMessage stores a user message for an avatar. The reply streams on the
// family socket, not in this response.
func (c *Client) SendMessage(ctx context.Context, fid, aid, body string) (Message, error) {
	var out struct {
		Message Message `json:"message"`
	}
	p := "/api/family/families/" + url.PathEscape(fid) + "/avatars/" + url.PathEscape(aid) + "/messages"
	err := c.do(ctx, http.MethodPost, p, map[string]string{"body": body}, &out)
	return out.Message, err
}
