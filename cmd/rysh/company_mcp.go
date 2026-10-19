// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A stdio MCP server over the bound company (design 037, second cut). It is
// what `##config load` registers as the MCP server "company", so every agent
// in the session gets read-only tools for the company's live data through the
// same bridge, approval gates and audit path as any other MCP server.
//
// Wire: newline-delimited JSON-RPC 2.0 on stdin/stdout, protocol 2024-11-05 —
// the shape rysh's own MCP client (internal/mcp) speaks. Tools only.
const (
	companyMCPProtocolVersion = "2024-11-05"
	companyMCPServerVersion   = "1"
	companySnapshotTTL        = 10 * time.Second
	companyToolDefaultLimit   = 50
	companyToolMaxLimit       = 500
)

type companyFetchFunc func(ctx context.Context) (*msg.CompanySnapshot, error)

// companyTools answers tools/list and tools/call. One snapshot serves every
// tool for a few seconds: an agent that asks three questions in a row does
// not fetch the company three times.
type companyTools struct {
	fetch companyFetchFunc
	now   func() time.Time

	mu     sync.Mutex
	cached *msg.CompanySnapshot
	at     time.Time
}

func newCompanyTools(fetch companyFetchFunc) *companyTools {
	return &companyTools{fetch: fetch, now: time.Now}
}

func (t *companyTools) snapshot(ctx context.Context) (*msg.CompanySnapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != nil && t.now().Sub(t.at) < companySnapshotTTL {
		return t.cached, nil
	}
	snap, err := t.fetch(ctx)
	if err != nil {
		return nil, err
	}
	t.cached, t.at = snap, t.now()
	return snap, nil
}

type companyToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var companyToolSpecs = []companyToolSpec{
	{
		Name:        "company_status",
		Description: "The company this session is bound to: name, site, status, the link state to the server, and how many orders, enquiries and services it has right now.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		Name:        "company_orders",
		Description: "Orders placed on the company's website, newest first, with customer, fulfilment, items, amount and payment status. Optional status filter (received, accepted, ready, completed, cancelled…) and limit.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"only orders in this status"},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"at most this many (default 50)"}},"additionalProperties":false}`),
	},
	{
		Name:        "company_enquiries",
		Description: "Enquiries from the company's website — contact messages, reservations, bookings, applications — newest first. Optional kind and status filters and limit.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","description":"only this kind (contact, reservation, booking, …)"},"status":{"type":"string","description":"only this status (new, read, replied, closed…)"},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"at most this many (default 50)"}},"additionalProperties":false}`),
	},
	{
		Name:        "company_activity",
		Description: "What happened recently: orders and enquiries created or updated, payments settled, provisioning progress — newest first. Optional limit.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":500,"description":"at most this many (default 50)"}},"additionalProperties":false}`),
	},
	{
		Name:        "company_services",
		Description: "The company's managed services and their state: mailbox, phone number, selected services and messaging channels.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
}

// call runs one tool. The text is JSON; isErr marks a failure the model should
// read as such (the server unreachable, the key revoked, an unknown tool).
func (t *companyTools) call(ctx context.Context, name string, args json.RawMessage) (text string, isErr bool) {
	var in struct {
		Status string `json:"status"`
		Kind   string `json:"kind"`
		Limit  int    `json:"limit"`
	}
	if len(bytes.TrimSpace(args)) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), true
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = companyToolDefaultLimit
	}
	if limit > companyToolMaxLimit {
		limit = companyToolMaxLimit
	}
	known := false
	for _, s := range companyToolSpecs {
		if s.Name == name {
			known = true
			break
		}
	}
	if !known {
		return fmt.Sprintf("unknown tool %q", name), true
	}
	snap, err := t.snapshot(ctx)
	if err != nil {
		return "company data unavailable: " + err.Error(), true
	}

	var out any
	switch name {
	case "company_status":
		out = map[string]any{
			"company":    snap.Company,
			"link":       snap.Link,
			"orders":     len(snap.Orders),
			"enquiries":  len(snap.Enquiries),
			"services":   len(snap.Services),
			"fetched_at": snap.FetchedAt,
		}
	case "company_orders":
		orders := make([]msg.CompanyOrder, 0, len(snap.Orders))
		for _, o := range snap.Orders {
			if in.Status != "" && !strings.EqualFold(o.Status, in.Status) {
				continue
			}
			orders = append(orders, o)
		}
		sort.SliceStable(orders, func(i, j int) bool { return orders[i].CreatedAt.After(orders[j].CreatedAt) })
		if len(orders) > limit {
			orders = orders[:limit]
		}
		out = map[string]any{"count": len(orders), "orders": orders}
	case "company_enquiries":
		list := make([]msg.CompanyEnquiry, 0, len(snap.Enquiries))
		for _, e := range snap.Enquiries {
			if in.Kind != "" && !strings.EqualFold(e.Kind, in.Kind) {
				continue
			}
			if in.Status != "" && !strings.EqualFold(e.Status, in.Status) {
				continue
			}
			list = append(list, e)
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
		if len(list) > limit {
			list = list[:limit]
		}
		out = map[string]any{"count": len(list), "enquiries": list}
	case "company_activity":
		list := append([]msg.CompanyActivity(nil), snap.Activity...)
		sort.SliceStable(list, func(i, j int) bool { return list[i].OccurredAt.After(list[j].OccurredAt) })
		if len(list) > limit {
			list = list[:limit]
		}
		out = map[string]any{"count": len(list), "activity": list}
	case "company_services":
		out = map[string]any{
			"email":    snap.Email,
			"phone":    snap.Phone,
			"services": snap.Services,
			"channels": snap.Channels,
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "could not encode the answer: " + err.Error(), true
	}
	return string(b), false
}

// --- JSON-RPC loop -----------------------------------------------------------

type companyRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type companyRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type companyRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *companyRPCError `json:"error,omitempty"`
}

// serveCompanyMCP reads requests from in until EOF and answers on out. It
// returns when stdin closes — the manager stopping the child — or on a read
// error. Notifications (no id) are consumed silently, as the protocol asks.
func serveCompanyMCP(in io.Reader, out io.Writer, tools *companyTools) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var wmu sync.Mutex
	write := func(resp companyRPCResponse) {
		resp.JSONRPC = "2.0"
		b, err := json.Marshal(resp)
		if err != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = out.Write(append(b, '\n'))
	}
	fail := func(id json.RawMessage, code int, message string) {
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		write(companyRPCResponse{ID: id, Error: &companyRPCError{Code: code, Message: message}})
	}

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req companyRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			fail(nil, -32700, "parse error: "+err.Error())
			continue
		}
		if len(req.ID) == 0 || bytes.Equal(bytes.TrimSpace(req.ID), []byte("null")) {
			continue // notification: initialized, cancelled, …
		}
		switch req.Method {
		case "initialize":
			write(companyRPCResponse{ID: req.ID, Result: map[string]any{
				"protocolVersion": companyMCPProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "rysh-company", "version": companyMCPServerVersion},
			}})
		case "ping":
			write(companyRPCResponse{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			write(companyRPCResponse{ID: req.ID, Result: map[string]any{"tools": companyToolSpecs}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
				fail(req.ID, -32602, "tools/call needs a tool name")
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			text, isErr := tools.call(ctx, p.Name, p.Arguments)
			cancel()
			write(companyRPCResponse{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}})
		default:
			fail(req.ID, -32601, "method not found: "+req.Method)
		}
	}
	return sc.Err()
}
