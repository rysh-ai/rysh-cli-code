// SPDX-License-Identifier: Apache-2.0

package mcpserver

// serverTemplate is the source of the generated MCP server. Placeholders
// (__BASE_URL__, __SERVER_NAME__, __AUTH_MODE__, __AUTH_KEY__, __OPS_LITERAL__)
// are substituted by the generator. The tool catalog is embedded from tools.json
// via go:embed. The emitted code must not contain backticks (this is a raw
// string literal).
const serverTemplate = `package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

//go:embed tools.json
var toolsJSON []byte

const protocolVersion = "2024-11-05"

var (
	serverName = __SERVER_NAME__
	authMode   = __AUTH_MODE__
	authKey    = __AUTH_KEY__
)

func baseURL() string {
	if v := os.Getenv("BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(__BASE_URL__, "/")
}

type opMeta struct {
	Method       string
	Path         string
	PathParams   []string
	QueryParams  []string
	HeaderParams []string
	HasBody      bool
}

var ops = map[string]opMeta{
__OPS_LITERAL__
}

type rpcRequest struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      json.RawMessage ` + "`json:\"id,omitempty\"`" + `
	Method  string          ` + "`json:\"method\"`" + `
	Params  json.RawMessage ` + "`json:\"params,omitempty\"`" + `
}

type rpcResponse struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      json.RawMessage ` + "`json:\"id,omitempty\"`" + `
	Result  any             ` + "`json:\"result,omitempty\"`" + `
	Error   *rpcError       ` + "`json:\"error,omitempty\"`" + `
}

type rpcError struct {
	Code    int    ` + "`json:\"code\"`" + `
	Message string ` + "`json:\"message\"`" + `
}

type contentBlock struct {
	Type string ` + "`json:\"type\"`" + `
	Text string ` + "`json:\"text\"`" + `
}

type toolResult struct {
	Content []contentBlock ` + "`json:\"content\"`" + `
	IsError bool           ` + "`json:\"isError,omitempty\"`" + `
}

func processRequest(req rpcRequest) *rpcResponse {
	if len(req.ID) == 0 {
		return nil // notification
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": serverName, "version": "1.0.0"},
		}
	case "tools/list":
		var tools []any
		_ = json.Unmarshal(toolsJSON, &tools)
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string         ` + "`json:\"name\"`" + `
			Arguments map[string]any ` + "`json:\"arguments\"`" + `
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			break
		}
		resp.Result = callOp(p.Name, p.Arguments)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func callOp(name string, args map[string]any) toolResult {
	op, ok := ops[name]
	if !ok {
		return errResult("unknown tool: " + name)
	}
	if args == nil {
		args = map[string]any{}
	}
	path := op.Path
	for _, p := range op.PathParams {
		path = strings.ReplaceAll(path, "{"+p+"}", url.PathEscape(toStr(args[p])))
	}
	u := baseURL() + path
	q := url.Values{}
	for _, p := range op.QueryParams {
		if v, ok := args[p]; ok {
			q.Set(p, toStr(v))
		}
	}
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}

	var body io.Reader
	if op.HasBody {
		consumed := map[string]bool{}
		for _, p := range op.PathParams {
			consumed[p] = true
		}
		for _, p := range op.QueryParams {
			consumed[p] = true
		}
		for _, p := range op.HeaderParams {
			consumed[p] = true
		}
		payload := map[string]any{}
		for k, v := range args {
			if !consumed[k] {
				payload[k] = v
			}
		}
		raw, _ := json.Marshal(payload)
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(op.Method, u, body)
	if err != nil {
		return errResult(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	if op.HasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, p := range op.HeaderParams {
		if v, ok := args[p]; ok {
			req.Header.Set(p, toStr(v))
		}
	}
	applyAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errResult("request failed: " + err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := string(data)
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil {
		text = buf.String()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, text))
	}
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

func applyAuth(req *http.Request) {
	switch authMode {
	case "apikey_header":
		if k := os.Getenv("API_KEY"); k != "" {
			req.Header.Set(authKey, k)
		}
	case "apikey_query":
		if k := os.Getenv("API_KEY"); k != "" {
			q := req.URL.Query()
			q.Set(authKey, k)
			req.URL.RawQuery = q.Encode()
		}
	case "bearer":
		if k := os.Getenv("API_KEY"); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	case "basic":
		u, p := os.Getenv("API_USER"), os.Getenv("API_PASS")
		if u != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u+":"+p)))
		}
	}
}

func errResult(msg string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: msg}}, IsError: true}
}

func toStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func main() {
	httpAddr := flag.String("http", "", "run as Streamable HTTP server on this address (e.g. :8080); default is stdio")
	flag.Parse()
	log.SetOutput(os.Stderr)

	if *httpAddr != "" {
		http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var req rpcRequest
			if err := json.Unmarshal(data, &req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			resp := processRequest(req)
			if resp == nil {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		log.Printf("%s MCP server (Streamable HTTP) on %s", serverName, *httpAddr)
		log.Fatal(http.ListenAndServe(*httpAddr, nil))
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		resp := processRequest(req)
		if resp == nil {
			continue
		}
		out, _ := json.Marshal(resp)
		fmt.Println(string(out))
	}
}
`
