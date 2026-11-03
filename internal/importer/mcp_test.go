// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/mcp"
)

// placeholders are the fixture values that stand in for secrets. None of them
// may ever reach a preview, a JSON plan or a written file. They are checked
// only for presence, never measured.
var placeholders = []string{
	"placeholder-value-1", "placeholder-value-2", "placeholder-value-3",
	"placeholder-value-4", "placeholder-value-5", "placeholder-value-6",
}

func assertNoPlaceholder(t *testing.T, what string, text []byte) {
	t.Helper()
	for _, v := range placeholders {
		if strings.Contains(string(text), v) {
			t.Errorf("%s carries a secret value from the fixture (%s)", what, "placeholder")
			return
		}
	}
	// Even the debug value of a non-secret-looking env var is not copied:
	// the import copies no env value at all.
	if strings.Contains(string(text), `"debug"`) {
		t.Errorf("%s copied an env value", what)
	}
}

func mcpPlan(t *testing.T, existing []byte) *Plan {
	t.Helper()
	p, err := ParseMCP(readFixture(t, "mcp-project.json"), existing, ".mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func entryNames(p *Plan) []string {
	var out []string
	for _, e := range p.MCP {
		out = append(out, e.Name)
	}
	return out
}

func TestParseMCP_Entries(t *testing.T) {
	p := mcpPlan(t, nil)
	if got, want := entryNames(p), []string{"docs", "events", "files", "tracker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imported servers = %q, want %q (sorted; leaky ones and broken ones left out)", got, want)
	}
	byName := map[string]MCPEntry{}
	for _, e := range p.MCP {
		byName[e.Name] = e
	}
	if e := byName["files"]; e.Transport != mcp.TransportStdio || e.Command != "npx" || len(e.Args) != 3 {
		t.Errorf("files = %+v", e)
	}
	if e := byName["docs"]; e.Transport != mcp.TransportHTTP || e.URL != "https://mcp.example.test/mcp" {
		t.Errorf("docs = %+v", e)
	}
	for _, need := range []string{"leaky", "leaky-url", "broken", "events"} {
		if !hasNote(p, `"`+need+`"`) {
			t.Errorf("no note explains server %q; notes = %q", need, p.Notes)
		}
	}
}

func TestParseMCP_SecretsByNameOnly(t *testing.T) {
	p := mcpPlan(t, nil)
	var names []string
	for _, s := range p.Secrets {
		names = append(names, s.Name)
	}
	want := []string{"FIXTURE_LOG_LEVEL", "FIXTURE_TRACKER_TOKEN", "Authorization", "--api-key", "token"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Errorf("secret name %q not listed; got %q", w, names)
		}
	}
	for _, s := range p.Secrets {
		if s.How == "" {
			t.Errorf("secret %q has no instruction for adding it to rysh", s.Name)
		}
	}
	js, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	assertNoPlaceholder(t, "preview", []byte(p.Text()))
	assertNoPlaceholder(t, "json", js)
	for _, c := range p.Changes("") {
		assertNoPlaceholder(t, c.Path, c.Data)
	}
}

func TestParseMCP_WritesValidStoreGatedByDefault(t *testing.T) {
	p := mcpPlan(t, nil)
	ch := p.Changes("")
	if len(ch) != 1 || ch[0].Path != ".rysh/mcp.json" {
		t.Fatalf("changes = %+v", ch)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".rysh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rysh", "mcp.json"), ch[0].Data, 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := mcp.LoadStore(root)
	if err != nil {
		t.Fatalf("written store does not load: %v", err)
	}
	if len(defs) != 4 {
		t.Fatalf("store has %d servers", len(defs))
	}
	for _, d := range defs {
		if err := d.Validate(); err != nil {
			t.Errorf("server %q invalid: %v", d.Name, err)
		}
		if !d.RequiresApproval {
			t.Errorf("imported server %q must gate every call until reviewed", d.Name)
		}
		if len(d.Env) != 0 || len(d.Headers) != 0 {
			t.Errorf("server %q carries env/headers: %+v", d.Name, d)
		}
	}
}

func TestParseMCP_KeepsExistingStoreUntouched(t *testing.T) {
	existing := []byte(`[
  {
    "name": "tracker",
    "transport": "stdio",
    "command": "my-own-tracker",
    "env": ["KEEP_ME=as-is"],
    "x_unknown_field": 7
  }
]
`)
	p := mcpPlan(t, existing)
	if got := entryNames(p); reflect.DeepEqual(got, []string{"docs", "events", "files", "tracker"}) {
		t.Fatalf("a server already in .rysh/mcp.json must not be re-imported: %q", got)
	}
	if !hasNote(p, "already") {
		t.Errorf("the skip must be noted; notes = %q", p.Notes)
	}
	ch := p.Changes("")
	var raw []map[string]any
	if err := json.Unmarshal(ch[0].Data, &raw); err != nil {
		t.Fatal(err)
	}
	first := raw[0]
	if first["command"] != "my-own-tracker" || first["x_unknown_field"] != float64(7) {
		t.Errorf("the existing entry changed: %v", first)
	}
	if env, _ := first["env"].([]any); len(env) != 1 || env[0] != "KEEP_ME=as-is" {
		t.Errorf("the existing entry's env changed: %v", first["env"])
	}
	if len(raw) != 4 {
		t.Errorf("store has %d entries, want 1 existing + 3 new", len(raw))
	}
}

func TestParseMCP_RefusesBrokenExistingStore(t *testing.T) {
	if _, err := ParseMCP(readFixture(t, "mcp-project.json"), []byte("[ not json"), ".mcp.json"); err == nil {
		t.Error("an unreadable .rysh/mcp.json must be refused, never overwritten")
	}
}

func TestParseMCP_ClaudeDesktopShape(t *testing.T) {
	p, err := ParseMCP(readFixture(t, "mcp-claude-desktop.json"), nil, "claude_desktop_config.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := entryNames(p); !reflect.DeepEqual(got, []string{"weather"}) {
		t.Errorf("servers = %q", got)
	}
	assertNoPlaceholder(t, "preview", []byte(p.Text()))
	if _, err := ParseMCP([]byte(`{"other": 1}`), nil, "x.json"); err == nil {
		t.Error("a file with no mcpServers must be an error")
	}
}

func TestMCPPreviewGolden(t *testing.T) {
	p := mcpPlan(t, nil)
	golden(t, "mcp.preview.txt", []byte(p.Text()))
	js, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "mcp.preview.json", js)
}
