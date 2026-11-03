// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/mcp"
)

// MCPEntry is one MCP server an import adds to .rysh/mcp.json. It carries no
// env and no headers: those are where credentials live, so the import copies
// none of them and lists their names instead.
type MCPEntry struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
}

// mcpSourceServer is one entry of a Claude Code .mcp.json or Claude Desktop
// config. Env and header VALUES are decoded only as far as their keys: the
// map values are json.RawMessage and are never read.
type mcpSourceServer struct {
	Type    string                     `json:"type"`
	Command string                     `json:"command"`
	Args    []string                   `json:"args"`
	URL     string                     `json:"url"`
	Env     map[string]json.RawMessage `json:"env"`
	Headers map[string]json.RawMessage `json:"headers"`
}

// credentialWord marks an argument flag or URL query key that carries a
// credential by name. It looks at names only, never at values.
var credentialWord = regexp.MustCompile(`(?i)(key|token|secret|passw|pwd|auth|credential|bearer|session)`)

// ParseMCP builds a plan that adds the servers of an MCP config (the
// `mcpServers` object of .mcp.json or claude_desktop_config.json) to the
// project's .rysh/mcp.json. existing is that file's current content (nil when
// absent); its entries are kept byte for byte and a server it already names is
// not imported again.
//
// What is never copied: env values, header values, a credential passed as an
// argument, a credential in a URL. A server whose only working form needs one
// of the last two is left out entirely, with a note: dropping the value would
// leave a command that cannot work, and keeping it would copy a secret.
// Imported servers are written with requires_approval, so every tool call is
// gated until the user has reviewed the server.
func ParseMCP(data, existing []byte, label string) (*Plan, error) {
	var doc struct {
		MCPServers map[string]mcpSourceServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	if doc.MCPServers == nil {
		return nil, fmt.Errorf("%s has no mcpServers object", label)
	}
	var have []json.RawMessage
	if len(strings.TrimSpace(string(existing))) > 0 {
		if err := json.Unmarshal(existing, &have); err != nil {
			return nil, fmt.Errorf("existing .rysh/mcp.json does not parse (%v); fix it before importing — it is never overwritten", err)
		}
	}
	taken := map[string]bool{}
	for _, raw := range have {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &d); err == nil {
			taken[d.Name] = true
		}
	}

	p := &Plan{Source: "mcp", Name: "", Inputs: []string{label}, mcpExisting: have}
	names := make([]string, 0, len(doc.MCPServers))
	for n := range doc.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s := doc.MCPServers[name]
		where := fmt.Sprintf("mcp server %q", name)
		for _, k := range sortedKeys(s.Env) {
			p.Secrets = append(p.Secrets, Secret{Name: k, Where: where + " env",
				How: "not copied (no env value is); export it in the shell that starts rysh — MCP servers inherit rysh's environment"})
		}
		for _, k := range sortedKeys(s.Headers) {
			p.Secrets = append(p.Secrets, Secret{Name: k, Where: where + " header",
				How: `not copied; add it under "headers" for this server in .rysh/mcp.json yourself`})
		}
		if taken[name] {
			p.note("server %q is already in .rysh/mcp.json; left unchanged, not imported again", name)
			continue
		}
		e := MCPEntry{Name: name}
		switch strings.ToLower(s.Type) {
		case "", "stdio":
			e.Transport = mcp.TransportStdio
			e.Command, e.Args = s.Command, s.Args
			if s.Command == "" && s.URL != "" {
				e.Transport, e.URL = mcp.TransportHTTP, s.URL
			}
		case "http", "streamable-http", "sse":
			e.Transport, e.URL = mcp.TransportHTTP, s.URL
			if strings.ToLower(s.Type) == "sse" {
				p.note("server %q uses the older SSE transport; rysh speaks Streamable HTTP — check the server supports it", name)
			}
		default:
			p.note("server %q: unknown transport %q; not imported", name, s.Type)
			continue
		}
		if flag := credentialArg(s.Args); flag != "" {
			p.Secrets = append(p.Secrets, Secret{Name: flag, Where: where + " argument",
				How: "not copied; add the server by hand with ##mcp add, passing the credential from rysh's secret store"})
			p.note("server %q passes a credential as an argument (%s); not imported", name, flag)
			continue
		}
		if e.URL != "" {
			if keys, ok := credentialURL(e.URL); !ok {
				for _, k := range keys {
					p.Secrets = append(p.Secrets, Secret{Name: k, Where: where + " url",
						How: "not copied; add the server by hand without a credential in its URL"})
				}
				p.note("server %q has a credential in its URL; not imported", name)
				continue
			}
		}
		def := mcp.ServerDef{Name: e.Name, Transport: e.Transport, Command: e.Command, Args: e.Args, URL: e.URL}
		if err := def.Validate(); err != nil {
			p.note("server %q is incomplete (%v); not imported", name, err)
			continue
		}
		p.MCP = append(p.MCP, e)
	}
	if len(p.MCP) > 0 {
		p.note("imported MCP servers are written with requires_approval: every tool call is gated until you review the server and remove it")
	}
	return p, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// credentialArg returns the first argument flag that names a credential
// (--api-key VALUE, --token=VALUE), or "".
func credentialArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flag, _, _ := strings.Cut(a, "=")
		if credentialWord.MatchString(strings.TrimLeft(flag, "-")) {
			return flag
		}
	}
	return ""
}

// credentialURL reports the credential-bearing parts of a URL by name: user
// info, and query keys that name a credential. ok is false when any is found.
func credentialURL(raw string) (names []string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, true // Validate reports a malformed URL
	}
	if u.User != nil {
		names = append(names, "URL user info")
	}
	for _, k := range sortedQueryKeys(u.Query()) {
		if credentialWord.MatchString(k) {
			names = append(names, k)
		}
	}
	return names, len(names) == 0
}

func sortedQueryKeys(q url.Values) []string {
	out := make([]string, 0, len(q))
	for k := range q {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mcpStore renders .rysh/mcp.json: the existing entries exactly as they were,
// then the imported ones, gated.
func (p *Plan) mcpStore() []byte {
	entries := append([]json.RawMessage{}, p.mcpExisting...)
	for _, e := range p.MCP {
		def := mcp.ServerDef{Name: e.Name, Transport: e.Transport, Command: e.Command, Args: e.Args, URL: e.URL, RequiresApproval: true}
		data, err := json.Marshal(def)
		if err != nil {
			continue // a ServerDef of strings always marshals
		}
		entries = append(entries, data)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil
	}
	return append(data, '\n')
}
