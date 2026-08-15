// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// ---------------------------------------------------------------------------
// A reusable in-memory MCP server implementing initialize / tools/list (with
// optional pagination) / tools/call, used by both the HTTP and stdio tests.
// ---------------------------------------------------------------------------

type fakeServer struct {
	tools     []Tool
	paginate  bool // when true, tools/list returns one tool per page
	callCount int
	mu        sync.Mutex
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		tools: []Tool{
			{
				Name:        "get_weather",
				Description: "Get weather for a city.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
			{
				Name:        "convert_units",
				Description: "Convert units.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"number"}}}`),
			},
		},
	}
}

// handle processes a single decoded JSON-RPC request and returns a response, or
// nil for a notification.
func (s *fakeServer) handle(req jsonrpcMessage) *jsonrpcMessage {
	if req.Method == notificationInitialized {
		return nil
	}
	resp := &jsonrpcMessage{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = mustJSON(initializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    serverCapabilities{Tools: &toolsCapability{ListChanged: true}},
			ServerInfo:      implementation{Name: "fake", Version: "1.0.0"},
		})
	case "tools/list":
		var p toolsListParams
		_ = json.Unmarshal(req.Params, &p)
		if s.paginate {
			resp.Result = mustJSON(s.pagedList(p.Cursor))
		} else {
			resp.Result = mustJSON(toolsListResult{Tools: s.tools})
		}
	case "tools/call":
		var p callToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "bad params"}
			break
		}
		s.mu.Lock()
		s.callCount++
		s.mu.Unlock()
		switch p.Name {
		case "get_weather":
			var args struct {
				City string `json:"city"`
			}
			_ = json.Unmarshal(p.Arguments, &args)
			resp.Result = mustJSON(callToolResult{Content: []contentBlock{{Type: "text", Text: "weather in " + args.City + ": sunny"}}})
		case "boom":
			resp.Result = mustJSON(callToolResult{Content: []contentBlock{{Type: "text", Text: "kaboom"}}, IsError: true})
		default:
			resp.Error = &rpcError{Code: -32601, Message: "unknown tool: " + p.Name}
		}
	case "ping":
		resp.Result = mustJSON(map[string]any{})
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// pagedList returns exactly one tool per page, advancing the cursor.
func (s *fakeServer) pagedList(cursor string) toolsListResult {
	idx := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &idx)
	}
	if idx >= len(s.tools) {
		return toolsListResult{}
	}
	res := toolsListResult{Tools: []Tool{s.tools[idx]}}
	if idx+1 < len(s.tools) {
		res.NextCursor = fmt.Sprintf("%d", idx+1)
	}
	return res
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// httpHandler adapts a fakeServer to a Streamable-HTTP endpoint.
func (s *fakeServer) httpHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcMessage
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if strings.Contains(string(body), `"initialize"`) {
			w.Header().Set("Mcp-Session-Id", "test-session-123")
		}
		resp := s.handle(req)
		w.Header().Set("Content-Type", "application/json")
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// serveStdio runs the fake server's read/respond loop over newline-delimited
// JSON, simulating an MCP stdio child. It returns when in reaches EOF.
func (s *fakeServer) serveStdio(in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonrpcMessage
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		resp := s.handle(req)
		if resp == nil {
			continue
		}
		b, _ := json.Marshal(resp)
		b = append(b, '\n')
		_, _ = out.Write(b)
	}
}

// ---------------------------------------------------------------------------
// HTTP transport / Manager end-to-end
// ---------------------------------------------------------------------------

func TestManagerHTTPEndToEnd(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs.httpHandler())
	defer srv.Close()

	dir := t.TempDir()
	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, dir)
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := mgr.AddServer(ctx, ServerDef{Name: "weather", Transport: TransportHTTP, URL: srv.URL})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if n != 2 {
		t.Fatalf("registered %d tools, want 2", n)
	}

	// Tools registered into the shared registry under the "<name>_" prefix.
	if _, ok := reg.Get("weather_get_weather"); !ok {
		t.Fatalf("weather_get_weather not registered; names=%v", reg.Names())
	}

	// The registered executor calls through to the server.
	exec, _ := reg.Get("weather_get_weather")
	out, err := exec.Execute(ctx, json.RawMessage(`{"city":"london"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Content, "weather in london: sunny") {
		t.Fatalf("unexpected content %q", out.Content)
	}

	// Definition was persisted to the store.
	defs, err := LoadStore(dir)
	if err != nil || len(defs) != 1 || defs[0].Name != "weather" {
		t.Fatalf("store not persisted: defs=%v err=%v", defs, err)
	}

	// Removing the server unregisters its tools and clears the store.
	if err := mgr.RemoveServer("weather"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	if _, ok := reg.Get("weather_get_weather"); ok {
		t.Fatalf("tool still registered after remove")
	}
	defs, _ = LoadStore(dir)
	if len(defs) != 0 {
		t.Fatalf("store not cleared after remove: %v", defs)
	}
}

func TestManagerHTTPApprovalAndError(t *testing.T) {
	fs := newFakeServer()
	fs.tools = append(fs.tools, Tool{Name: "boom", Description: "always fails", InputSchema: json.RawMessage(`{"type":"object"}`)})
	srv := httptest.NewServer(fs.httpHandler())
	defer srv.Close()

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, t.TempDir())
	defer mgr.Close()
	ctx := context.Background()

	if _, err := mgr.AddServer(ctx, ServerDef{Name: "w", Transport: TransportHTTP, URL: srv.URL, RequiresApproval: true}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	exec, ok := reg.Get("w_boom")
	if !ok {
		t.Fatalf("w_boom not registered")
	}
	if !exec.RequiresApproval(nil) {
		t.Fatalf("expected RequiresApproval=true from server policy")
	}

	// A tool that reports isError surfaces as ToolOutput.Error, not a Go error.
	out, err := exec.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if out.Error == "" || !strings.Contains(out.Error, "kaboom") {
		t.Fatalf("expected isError surfaced in Error, got %+v", out)
	}
}

func TestClientPaginationHTTP(t *testing.T) {
	fs := newFakeServer()
	fs.paginate = true
	srv := httptest.NewServer(fs.httpHandler())
	defer srv.Close()

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, t.TempDir())
	defer mgr.Close()

	n, err := mgr.AddServer(context.Background(), ServerDef{Name: "p", Transport: TransportHTTP, URL: srv.URL})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if n != 2 {
		t.Fatalf("pagination collected %d tools, want 2", n)
	}
}

func TestMaxToolsCap(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs.httpHandler())
	defer srv.Close()

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, t.TempDir())
	defer mgr.Close()

	n, err := mgr.AddServer(context.Background(), ServerDef{Name: "c", Transport: TransportHTTP, URL: srv.URL, MaxTools: 1})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if n != 1 {
		t.Fatalf("MaxTools=1 registered %d tools, want 1", n)
	}
	status := mgr.List()
	if len(status) != 1 || status[0].Discovered != 2 || status[0].Registered != 1 {
		t.Fatalf("status mismatch: %+v", status)
	}
}

// ---------------------------------------------------------------------------
// stdio transport end-to-end (in-memory pipes, no subprocess)
// ---------------------------------------------------------------------------

func TestStdioClientEndToEnd(t *testing.T) {
	fs := newFakeServer()

	clientReader, serverWriter := io.Pipe() // server -> client
	serverReader, clientWriter := io.Pipe() // client -> server

	go fs.serveStdio(serverReader, serverWriter)

	tr := newStdioPipe("fake", clientWriter, clientReader, func() error {
		_ = clientWriter.Close()
		_ = serverWriter.Close()
		return nil
	})
	client := newClient("fake", tr)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	name, version := client.ServerInfo()
	if name != "fake" || version != "1.0.0" {
		t.Fatalf("ServerInfo = %q/%q", name, version)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("ListTools returned %d, want 2", len(tools))
	}

	res, err := client.CallTool(ctx, "get_weather", json.RawMessage(`{"city":"tokyo"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "tokyo") {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestStdioContextCancel(t *testing.T) {
	// A server that consumes requests but never replies: roundTrip must unblock
	// when ctx is cancelled. The server side drains writes (so the request send
	// succeeds) but sends nothing back.
	clientReader, serverWriter := io.Pipe() // server -> client (never written)
	serverReader, clientWriter := io.Pipe() // client -> server
	go io.Copy(io.Discard, serverReader)    // drain requests, never respond

	tr := newStdioPipe("silent", clientWriter, clientReader, func() error {
		_ = clientWriter.Close()
		_ = serverWriter.Close() // unblock the reader loop on close
		return nil
	})
	client := newClient("silent", tr)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := client.ListTools(ctx); err == nil {
		t.Fatalf("expected context error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Parsing, store, sanitization
// ---------------------------------------------------------------------------

func TestParseAddArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, d ServerDef)
	}{
		{
			name: "http basic",
			args: []string{"weather", "http", "http://localhost:8081/mcp"},
			check: func(t *testing.T, d ServerDef) {
				if d.Transport != TransportHTTP || d.URL != "http://localhost:8081/mcp" {
					t.Fatalf("bad http def: %+v", d)
				}
			},
		},
		{
			name: "http with flags",
			args: []string{"api", "http", "https://x/mcp", "--header", "Authorization:Bearer t", "--approve", "--max-tools", "10"},
			check: func(t *testing.T, d ServerDef) {
				if !d.RequiresApproval || d.MaxTools != 10 || d.Headers["Authorization"] != "Bearer t" {
					t.Fatalf("bad flags: %+v", d)
				}
			},
		},
		{
			name: "stdio with args",
			args: []string{"fs", "stdio", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp", "--approve"},
			check: func(t *testing.T, d ServerDef) {
				if d.Transport != TransportStdio || d.Command != "npx" || len(d.Args) != 3 || !d.RequiresApproval {
					t.Fatalf("bad stdio def: %+v", d)
				}
			},
		},
		{
			name: "stdio with prefix override empty",
			args: []string{"x", "stdio", "mytool", "--prefix", "p_"},
			check: func(t *testing.T, d ServerDef) {
				if d.Prefix == nil || *d.Prefix != "p_" {
					t.Fatalf("prefix not parsed: %+v", d)
				}
			},
		},
		{name: "missing transport", args: []string{"only"}, wantErr: true},
		{name: "bad transport", args: []string{"n", "ftp", "x"}, wantErr: true},
		{name: "http no url", args: []string{"n", "http"}, wantErr: true},
		{name: "stdio no cmd", args: []string{"n", "stdio"}, wantErr: true},
		{name: "unknown flag", args: []string{"n", "http", "http://x", "--nope"}, wantErr: true},
		{name: "bad header", args: []string{"n", "http", "http://x", "--header", "noseparator"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseAddArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got def %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, d)
			}
		})
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file -> empty, no error.
	defs, err := LoadStore(dir)
	if err != nil || len(defs) != 0 {
		t.Fatalf("empty load: defs=%v err=%v", defs, err)
	}

	a := ServerDef{Name: "a", Transport: TransportHTTP, URL: "http://a/mcp"}
	b := ServerDef{Name: "b", Transport: TransportStdio, Command: "b"}
	if err := SaveStore(dir, []ServerDef{a, b}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".rysh", "mcp.json")); statErr != nil {
		t.Fatalf("store file not created: %v", statErr)
	}

	defs, err = LoadStore(dir)
	if err != nil || len(defs) != 2 {
		t.Fatalf("reload: defs=%v err=%v", defs, err)
	}

	// upsert replaces by name; remove drops by name.
	defs = upsertDef(defs, ServerDef{Name: "a", Transport: TransportHTTP, URL: "http://a2/mcp"})
	if len(defs) != 2 || defs[0].URL != "http://a2/mcp" {
		t.Fatalf("upsert failed: %+v", defs)
	}
	defs, removed := removeDef(defs, "b")
	if !removed || len(defs) != 1 {
		t.Fatalf("remove failed: removed=%v defs=%+v", removed, defs)
	}
}

func TestValidate(t *testing.T) {
	bad := []ServerDef{
		{Name: "", Transport: TransportHTTP, URL: "http://x"},
		{Name: "n", Transport: TransportHTTP, URL: "ftp://x"},
		{Name: "n", Transport: TransportHTTP},
		{Name: "n", Transport: TransportStdio},
		{Name: "n", Transport: "weird"},
		{Name: "n"},
	}
	for i, d := range bad {
		if err := d.Validate(); err == nil {
			t.Fatalf("case %d expected validation error for %+v", i, d)
		}
	}
	good := ServerDef{Name: "n", Transport: TransportStdio, Command: "x"}
	if err := good.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSanitizeToolName(t *testing.T) {
	cases := map[string]string{
		"get_weather":   "get_weather",
		"weird name!":   "weird_name_",
		"a/b:c":         "a_b_c",
		"":              "tool",
		"keep-dash_ok9": "keep-dash_ok9",
	}
	for in, want := range cases {
		if got := sanitizeToolName(in); got != want {
			t.Fatalf("sanitizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 200)
	if got := sanitizeToolName(long); len(got) != 64 {
		t.Fatalf("long name not truncated to 64: %d", len(got))
	}
}

func TestPrefixResolution(t *testing.T) {
	def := ServerDef{Name: "srv"}
	if def.prefix() != "srv_" {
		t.Fatalf("default prefix = %q", def.prefix())
	}
	empty := ""
	def.Prefix = &empty
	if def.prefix() != "" {
		t.Fatalf("explicit empty prefix not honored: %q", def.prefix())
	}
}

func TestParseHTTPResultSSE(t *testing.T) {
	// A Streamable-HTTP server may reply with text/event-stream. The matching
	// response (by id) must be extracted from the SSE frames.
	body := []byte(": keep-alive\n" +
		"event: message\n" +
		`data: {"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"t","description":"d","inputSchema":{}}]}}` + "\n\n")
	m, err := parseHTTPResult("text/event-stream; charset=utf-8", body, "7")
	if err != nil {
		t.Fatalf("parseHTTPResult SSE: %v", err)
	}
	var res toolsListResult
	if err := json.Unmarshal(m.Result, &res); err != nil {
		t.Fatalf("decode SSE result: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "t" {
		t.Fatalf("unexpected SSE tools: %+v", res.Tools)
	}
}

func TestParseHTTPResultJSON(t *testing.T) {
	m, err := parseHTTPResult("application/json", []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), "1")
	if err != nil || m == nil {
		t.Fatalf("parseHTTPResult JSON: %v", err)
	}
	if m.Error != nil {
		t.Fatalf("unexpected error: %v", m.Error)
	}
}

func TestRenderContent(t *testing.T) {
	got := renderContent([]contentBlock{
		{Type: "text", Text: "hello"},
		{Type: "image", MIMEType: "image/png", Data: "abcd"},
	})
	if !strings.Contains(got, "hello") || !strings.Contains(got, "image/png") {
		t.Fatalf("renderContent = %q", got)
	}
}
