// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func fixtureSnapshot() *msg.CompanySnapshot {
	t0 := time.Date(2026, 9, 18, 17, 0, 0, 0, time.UTC)
	return &msg.CompanySnapshot{
		Company: msg.CompanyInfo{ID: "c1", Name: "Pera Bistro", Slug: "pera-bistro", FQDN: "pera-bistro.dns.rysh.ai", Status: "active"},
		Orders: []msg.CompanyOrder{
			{ID: "o1", Reference: "AAA11", CustomerName: "Ayse", Status: "received", AmountPence: 3700, Currency: "gbp", CreatedAt: t0},
			{ID: "o2", Reference: "BBB22", CustomerName: "Tom", Status: "completed", AmountPence: 1650, Currency: "gbp", CreatedAt: t0.Add(time.Hour)},
			{ID: "o3", Reference: "CCC33", CustomerName: "Zoe", Status: "received", AmountPence: 600, Currency: "gbp", CreatedAt: t0.Add(2 * time.Hour)},
		},
		Enquiries: []msg.CompanyEnquiry{
			{ID: "e1", Reference: "RES1", Kind: "reservation", Status: "new", Name: "Deniz", CreatedAt: t0},
			{ID: "e2", Reference: "CON1", Kind: "contact", Status: "new", Name: "Ece", CreatedAt: t0.Add(time.Minute)},
		},
		Activity: []msg.CompanyActivity{{Kind: "order", Action: "created", Reference: "AAA11", Summary: "Order AAA11 received", OccurredAt: t0}},
		Link:     msg.CompanyLink{Connected: true, Live: true, LastFetch: t0},
		FetchedAt: t0,
	}
}

// rpc drives serveCompanyMCP over pipes the way the manager's stdio transport
// does: one JSON object per line in, one per line out.
type rpc struct {
	in  io.WriteCloser
	out *bufio.Scanner
}

func startServer(t *testing.T, fetch companyFetchFunc) *rpc {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveCompanyMCP(inR, outW, newCompanyTools(fetch)); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("server did not stop when stdin closed")
		}
	})
	return &rpc{in: inW, out: bufio.NewScanner(outR)}
}

func (r *rpc) send(t *testing.T, line string) map[string]any {
	t.Helper()
	if _, err := io.WriteString(r.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
	if !r.out.Scan() {
		t.Fatalf("no reply to %s", line)
	}
	var reply map[string]any
	if err := json.Unmarshal(r.out.Bytes(), &reply); err != nil {
		t.Fatalf("reply is not JSON: %v: %s", err, r.out.Text())
	}
	return reply
}

func TestCompanyMCPHandshakeAndTools(t *testing.T) {
	calls := 0
	r := startServer(t, func(context.Context) (*msg.CompanySnapshot, error) { calls++; return fixtureSnapshot(), nil })

	init := r.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != companyMCPProtocolVersion {
		t.Fatalf("initialize: %v", init)
	}
	// The initialized notification has no id and must draw no reply; the
	// next request's reply is the next line.
	if _, err := io.WriteString(r.in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	list := r.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	want := "company_status company_orders company_enquiries company_activity company_services"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools: %v", names)
	}
	for _, tool := range tools {
		if _, ok := tool.(map[string]any)["inputSchema"].(map[string]any); !ok {
			t.Fatalf("tool without an input schema: %v", tool)
		}
	}

	// Filtered, newest first, and the snapshot is fetched once for both calls.
	orders := r.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"company_orders","arguments":{"status":"received"}}}`)
	content := orders["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	var payload struct {
		Count  int `json:"count"`
		Orders []struct {
			Reference string `json:"reference"`
		} `json:"orders"`
	}
	if err := json.Unmarshal([]byte(content["text"].(string)), &payload); err != nil {
		t.Fatalf("orders text is not JSON: %v", err)
	}
	if payload.Count != 2 || payload.Orders[0].Reference != "CCC33" || payload.Orders[1].Reference != "AAA11" {
		t.Fatalf("orders: %+v", payload)
	}
	if isErr, _ := orders["result"].(map[string]any)["isError"].(bool); isErr {
		t.Fatalf("orders marked as error: %v", orders)
	}
	status := r.send(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"company_status","arguments":{}}}`)
	text := status["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"Pera Bistro"`) || !strings.Contains(text, `"live": true`) {
		t.Fatalf("status: %s", text)
	}
	if calls != 1 {
		t.Fatalf("snapshot fetched %d times for two calls inside the TTL", calls)
	}

	enq := r.send(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"company_enquiries","arguments":{"kind":"reservation"}}}`)
	if s := enq["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(s, `"RES1"`) || strings.Contains(s, `"CON1"`) {
		t.Fatalf("enquiries filter: %s", s)
	}

	unknown := r.send(t, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"company_delete_everything","arguments":{}}}`)
	if isErr, _ := unknown["result"].(map[string]any)["isError"].(bool); !isErr {
		t.Fatalf("an unknown tool must answer isError: %v", unknown)
	}
	missing := r.send(t, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if missing["error"] == nil {
		t.Fatalf("unknown method must be a JSON-RPC error: %v", missing)
	}
	bad := r.send(t, `{not json`)
	if bad["error"] == nil {
		t.Fatalf("a malformed line must be answered with a parse error: %v", bad)
	}
	pong := r.send(t, `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if pong["result"] == nil {
		t.Fatalf("ping: %v", pong)
	}
}

func TestCompanyMCPReportsFetchFailuresAsToolErrors(t *testing.T) {
	r := startServer(t, func(context.Context) (*msg.CompanySnapshot, error) { return nil, errors.New("key refused") })
	reply := r.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"company_status","arguments":{}}}`)
	result := reply["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("a failed fetch must be a tool error, not a silent empty answer: %v", reply)
	}
	if text := result["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "key refused") {
		t.Fatalf("the model must see why: %s", text)
	}
}

func TestCompanyToolsSnapshotTTL(t *testing.T) {
	now := time.Date(2026, 9, 18, 17, 0, 0, 0, time.UTC)
	calls := 0
	tools := newCompanyTools(func(context.Context) (*msg.CompanySnapshot, error) { calls++; return fixtureSnapshot(), nil })
	tools.now = func() time.Time { return now }
	if _, err := tools.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(companySnapshotTTL / 2)
	_, _ = tools.snapshot(context.Background())
	if calls != 1 {
		t.Fatalf("inside the TTL: %d fetches", calls)
	}
	now = now.Add(companySnapshotTTL)
	_, _ = tools.snapshot(context.Background())
	if calls != 2 {
		t.Fatalf("after the TTL: %d fetches", calls)
	}
}
