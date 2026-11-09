// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
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
	companyLedgerMaxLimit     = 200 // the server's page cap (accountant design note §4)
)

type companyFetchFunc func(ctx context.Context) (*msg.CompanySnapshot, error)

// companyTools answers tools/list and tools/call. One snapshot serves every
// tool for a few seconds: an agent that asks three questions in a row does
// not fetch the company three times.
type companyTools struct {
	fetch companyFetchFunc
	books companyBooks // nil: the bookkeeping tools answer "not available"
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
	{
		Name:        "company_deadlines",
		Description: "The company's filing deadlines from Companies House: annual accounts and confirmation statement, each with next_due, next_made_up_to and overdue. {linked:false} when no company number is linked yet.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		Name:        "company_ledger",
		Description: "The owner's bookkeeping ledger, newest first: entries (entry_date, direction in/out, amount_pence, currency, category, note) and totals over the whole filtered range, all amounts in integer pence. Read-only. Optional from/to dates (YYYY-MM-DD, inclusive), category, direction and limit; for the next page pass before = the previous page's next_before.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","description":"first date, YYYY-MM-DD"},"to":{"type":"string","description":"last date, YYYY-MM-DD"},"category":{"type":"string","description":"only this category key"},"direction":{"type":"string","enum":["in","out"]},"limit":{"type":"integer","minimum":1,"maximum":200,"description":"at most this many entries (server default 100)"},"before":{"type":"string","description":"next_before from the previous page"}},"additionalProperties":false}`),
	},
	{
		Name:        "company_ledger_summary",
		Description: "Monthly totals of the owner's bookkeeping ledger for the last N calendar months (UTC), oldest first: in_pence, out_pence and net_pence per month (every month listed, empty ones as 0) and totals by category. Read-only. Optional months (1-24, default 12).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"months":{"type":"integer","minimum":1,"maximum":24,"description":"how many calendar months, ending with this one (default 12)"}},"additionalProperties":false}`),
	},
}

// companyBooks reads the accountant routes for the bound company (AC4).
type companyBooks interface {
	Deadlines(ctx context.Context) (json.RawMessage, error)
	Ledger(ctx context.Context, q url.Values) (json.RawMessage, error)
}

var (
	companyCategoryRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	// before is the server's opaque next_before cursor; it is echoed back, so
	// only a plain token shape is accepted.
	companyCursorRe = regexp.MustCompile(`^[A-Za-z0-9:._~+-]{1,200}$`)
)

// ledgerQuery validates company_ledger's arguments into the server's query.
// Unknown fields are refused, so a typo is an error the model sees, not a
// silently unfiltered page.
func ledgerQuery(args json.RawMessage) (url.Values, error) {
	var in struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Category  string `json:"category"`
		Direction string `json:"direction"`
		Limit     int    `json:"limit"`
		Before    string `json:"before"`
	}
	if len(bytes.TrimSpace(args)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, err
		}
	}
	q := url.Values{}
	for _, d := range []struct{ key, val string }{{"from", in.From}, {"to", in.To}} {
		if d.val == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", d.val); err != nil {
			return nil, fmt.Errorf("%s must be a date as YYYY-MM-DD", d.key)
		}
		q.Set(d.key, d.val)
	}
	if in.Category != "" {
		if !companyCategoryRe.MatchString(in.Category) {
			return nil, errors.New("category must be a category key (lowercase letters, digits, _ or -)")
		}
		q.Set("category", in.Category)
	}
	switch in.Direction {
	case "":
	case "in", "out":
		q.Set("direction", in.Direction)
	default:
		return nil, errors.New(`direction must be "in" or "out"`)
	}
	if in.Before != "" {
		if !companyCursorRe.MatchString(in.Before) {
			return nil, errors.New("before must be the next_before value of the previous page")
		}
		q.Set("before", in.Before)
	}
	switch {
	case in.Limit < 0:
		return nil, errors.New("limit must be positive")
	case in.Limit > companyLedgerMaxLimit:
		q.Set("limit", fmt.Sprint(companyLedgerMaxLimit))
	case in.Limit > 0:
		q.Set("limit", fmt.Sprint(in.Limit))
	}
	return q, nil
}

// companyBooksSummary is the optional monthly-summary read (AC7).
type companyBooksSummary interface {
	Summary(ctx context.Context, months int) (json.RawMessage, error)
}

const (
	companySummaryDefaultMonths = 12
	companySummaryMaxMonths     = 24 // the server's cap
)

// summaryMonths validates company_ledger_summary's arguments.
func summaryMonths(args json.RawMessage) (int, error) {
	var in struct {
		Months *int `json:"months"`
	}
	if len(bytes.TrimSpace(args)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return 0, err
		}
	}
	if in.Months == nil {
		return companySummaryDefaultMonths, nil
	}
	if *in.Months < 1 || *in.Months > companySummaryMaxMonths {
		return 0, fmt.Errorf("months must be between 1 and %d", companySummaryMaxMonths)
	}
	return *in.Months, nil
}

// labelImportedNotes wraps the note of every ledger entry whose source is
// "csv" as {"untrusted_imported_text": "..."}: it is third-party text from a
// bank export, and the model must read it as data, never as an instruction
// (security-lead's AC8 pre-read P1). Manual notes stay plain strings; every
// other field, numbers included, round-trips unchanged.
func labelImportedNotes(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var page map[string]any
	if err := dec.Decode(&page); err != nil {
		return nil, fmt.Errorf("the server answered with invalid JSON")
	}
	entries, _ := page["entries"].([]any)
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok || entry["source"] != "csv" {
			continue
		}
		if note, ok := entry["note"].(string); ok && note != "" {
			entry["note"] = map[string]any{"untrusted_imported_text": note}
		}
	}
	return json.Marshal(page)
}

// callBooks runs company_deadlines / company_ledger / company_ledger_summary. They read the server on
// every call (no snapshot) and pass its JSON through.
func (t *companyTools) callBooks(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	if t.books == nil {
		return "the bookkeeping tools are not available in this session", true
	}
	var raw json.RawMessage
	var err error
	if name == "company_deadlines" {
		raw, err = t.books.Deadlines(ctx)
	} else if name == "company_ledger_summary" {
		sb, ok := t.books.(companyBooksSummary)
		if !ok {
			return "the monthly summary is not available in this session", true
		}
		months, merr := summaryMonths(args)
		if merr != nil {
			return "invalid arguments: " + merr.Error(), true
		}
		raw, err = sb.Summary(ctx, months)
	} else {
		q, qerr := ledgerQuery(args)
		if qerr != nil {
			return "invalid arguments: " + qerr.Error(), true
		}
		raw, err = t.books.Ledger(ctx, q)
		if err == nil {
			raw, err = labelImportedNotes(raw)
		}
	}
	switch {
	case errors.Is(err, company.ErrBooksNotEnabled):
		return "the bookkeeping assistant is not enabled for this company: the owner turns on " +
			"'Bookkeeping assistant' in the company's services on rysh.ai", true
	case err != nil:
		return "company books unavailable: " + err.Error(), true
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return "the server answered with invalid JSON", true
	}
	return buf.String(), false
}

// call runs one tool. The text is JSON; isErr marks a failure the model should
// read as such (the server unreachable, the key revoked, an unknown tool).
func (t *companyTools) call(ctx context.Context, name string, args json.RawMessage) (text string, isErr bool) {
	if name == "company_deadlines" || name == "company_ledger" || name == "company_ledger_summary" {
		return t.callBooks(ctx, name, args)
	}
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
// error.
func serveCompanyMCP(in io.Reader, out io.Writer, tools *companyTools) error {
	return serveStdioMCP(in, out, stdioMCPServer{
		name:        "rysh-company",
		tools:       func() any { return companyToolSpecs },
		call:        tools.call,
		callTimeout: 30 * time.Second,
	})
}

// stdioMCPServer is what one MCP server contributes to the shared stdio loop:
// its name, the tools it lists, and how a call runs. The company server and
// `rysh mcp serve` (design 067) both run on serveStdioMCP, so there is one
// JSON-RPC loop to get right.
type stdioMCPServer struct {
	name  string
	tools func() any
	call  func(ctx context.Context, name string, args json.RawMessage) (text string, isErr bool)
	// callTimeout bounds one tools/call. A tool that waits (wait_for_pane_state)
	// applies its own, shorter deadline inside this one.
	callTimeout time.Duration
}

// serveStdioMCP reads requests from in until EOF and answers on out.
// Notifications (no id) are consumed silently, as the protocol asks. Each
// tools/call runs on its own goroutine, so a long wait does not hold up the
// calls behind it; the loop waits for calls in flight before it returns.
func serveStdioMCP(in io.Reader, out io.Writer, srv stdioMCPServer) error {
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
	timeout := srv.callTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var inflight sync.WaitGroup
	defer inflight.Wait()

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
				"serverInfo":      map[string]any{"name": srv.name, "version": companyMCPServerVersion},
			}})
		case "ping":
			write(companyRPCResponse{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			write(companyRPCResponse{ID: req.ID, Result: map[string]any{"tools": srv.tools()}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
				fail(req.ID, -32602, "tools/call needs a tool name")
				continue
			}
			id := append(json.RawMessage(nil), req.ID...)
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				text, isErr := srv.call(ctx, p.Name, p.Arguments)
				cancel()
				write(companyRPCResponse{ID: id, Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
					"isError": isErr,
				}})
			}()
		default:
			fail(req.ID, -32601, "method not found: "+req.Method)
		}
	}
	return sc.Err()
}
