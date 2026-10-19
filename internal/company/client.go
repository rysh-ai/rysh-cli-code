// SPDX-License-Identifier: Apache-2.0

// Package company follows one AI-native company on the upstream rysh-server
// for a session bound to it (design 037, "Company Connect"): a REST client
// that reads the company's state with the read-only workspace key, and a
// follower that keeps a snapshot fresh from live NATS hints and a poll.
package company

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

var (
	// ErrUnauthorized is the server refusing the key. It is the one error the
	// follower treats specially: the key was revoked from the dashboard (or the
	// company was deleted), and polling harder will not help.
	ErrUnauthorized = errors.New("the server rejected this company's key — reconnect from the dashboard")
	errNotFound     = errors.New("not found")
)

// Client reads a company over the owner routes with a connect key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	return nil
}

// progressEvent is the provisioning envelope the company GET replays; it is
// folded into the activity list so the view shows a build in flight.
type progressEvent struct {
	Stage      string    `json:"stage"`
	Status     string    `json:"status"`
	Message    string    `json:"message"`
	ServiceKey string    `json:"service_key"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Fetch reads the whole company. The company record itself is required;
// every other resource is best-effort — a failure there is recorded in
// Link.LastError and the snapshot still carries what did load, so one broken
// endpoint never blanks the view. An authorization failure anywhere is
// returned as ErrUnauthorized, because it means the key, not the endpoint.
func (c *Client) Fetch(ctx context.Context, companyID string) (*msg.CompanySnapshot, error) {
	base := "/api/ai-native-companies/" + companyID
	var head struct {
		Company msg.CompanyInfo `json:"company"`
		Events  []progressEvent `json:"events"`
	}
	if err := c.get(ctx, base, &head); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	snap := &msg.CompanySnapshot{Company: head.Company, FetchedAt: time.Now().UTC()}
	snap.Company.ServerURL = c.BaseURL
	if snap.Company.FQDN != "" {
		snap.Company.SiteURL = "https://" + snap.Company.FQDN
	}
	for _, ev := range head.Events {
		summary := ev.Message
		if summary == "" {
			summary = ev.Stage
		}
		if ev.ServiceKey != "" {
			summary = ev.ServiceKey + ": " + summary
		}
		snap.Activity = append(snap.Activity, msg.CompanyActivity{
			Kind: "progress", Action: ev.Stage, Status: ev.Status, Summary: summary, OccurredAt: ev.OccurredAt,
		})
	}

	var firstErr error
	note := func(err error) error {
		if err == nil || errors.Is(err, errNotFound) {
			return nil
		}
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		if firstErr == nil {
			firstErr = err
		}
		return nil
	}

	var orders struct {
		Orders []msg.CompanyOrder `json:"orders"`
	}
	if err := note(c.get(ctx, base+"/orders", &orders)); err != nil {
		return nil, err
	}
	snap.Orders = orders.Orders

	var enquiries struct {
		Enquiries []msg.CompanyEnquiry `json:"enquiries"`
	}
	if err := note(c.get(ctx, base+"/enquiries", &enquiries)); err != nil {
		return nil, err
	}
	snap.Enquiries = enquiries.Enquiries

	var email struct {
		Email *msg.CompanyEmail `json:"email"`
	}
	if err := note(c.get(ctx, base+"/email", &email)); err != nil {
		return nil, err
	}
	snap.Email = email.Email

	var phone map[string]any
	if err := note(c.get(ctx, base+"/phone", &phone)); err != nil {
		return nil, err
	}
	snap.Phone = pickPhone(phone)

	var services struct {
		Services []msg.CompanyService `json:"services"`
	}
	if err := note(c.get(ctx, base+"/services", &services)); err != nil {
		return nil, err
	}
	snap.Services = services.Services

	var channels struct {
		Channels []msg.CompanyChannel `json:"channels"`
	}
	if err := note(c.get(ctx, base+"/channels", &channels)); err != nil {
		return nil, err
	}
	snap.Channels = channels.Channels

	snap.Link.Connected = true
	snap.Link.LastFetch = snap.FetchedAt
	if firstErr != nil {
		snap.Link.LastError = firstErr.Error()
	}
	return snap, nil
}

// pickPhone reads the few fields the view shows out of the phone reply,
// whatever envelope it arrived in ({"phone": {...}} or the object itself).
func pickPhone(raw map[string]any) *msg.CompanyPhone {
	if len(raw) == 0 {
		return nil
	}
	if inner, ok := raw["phone"].(map[string]any); ok {
		raw = inner
	}
	if len(raw) == 0 {
		return nil
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	out := &msg.CompanyPhone{
		Number:  str("e164", "phone_number", "number", "friendly_name"),
		State:   str("state", "status", "lifecycle_state"),
		Message: str("status_message", "message", "next_action"),
	}
	if out.Number == "" && out.State == "" && out.Message == "" {
		return nil
	}
	return out
}
