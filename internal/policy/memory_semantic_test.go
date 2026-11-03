// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"strings"
	"testing"
)

// Design 010 §3 S4: semantic memory is OFF unless a policy turns it on, and
// strictest wins against the org file, like the rest of memory:.
func TestMemorySemanticRules(t *testing.T) {
	on := `memory:
  semantic:
    enabled: true
    model: nomic-embed-text
    dim: 768
`
	cases := []struct {
		name, project, org string
		want               SemanticRules
	}{
		{"absent", "", "", SemanticRules{}},
		{"on", on, "", SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768}},
		{"org-off-wins", on, "memory:\n  semantic:\n    enabled: false\n", SemanticRules{}},
		{"memory-off-wins", on + "  enabled: false\n", "", SemanticRules{}},
		// The project file ships with the repo: alone it can never open a
		// remote embedder (a cloned repo would receive the user's memories).
		{"project-cannot-allow-remote", on + "    embedder_url: https://embed.example.com\n    allow_remote: true\n", "",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768}},
		{"project-remote-url-dropped", on + "    embedder_url: http://10.1.2.3:11434\n", "",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768}},
		{"project-loopback-url-kept", on + "    embedder_url: http://127.0.0.1:11500\n", "",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768, EmbedderURL: "http://127.0.0.1:11500"}},
		{"org-allows-remote", on, "memory:\n  semantic:\n    embedder_url: https://embed.example.com\n    allow_remote: true\n",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768, EmbedderURL: "https://embed.example.com", AllowRemote: true}},
		{"org-remote-url-project-allow-ignored", on + "    allow_remote: true\n", "memory:\n  semantic:\n    embedder_url: https://embed.example.com\n",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768, EmbedderURL: "https://embed.example.com"}},
		{"project-turns-off-org-remote", on + "    allow_remote: false\n", "memory:\n  semantic:\n    embedder_url: https://embed.example.com\n    allow_remote: true\n",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768, EmbedderURL: "https://embed.example.com"}},
		{"org-forbids-remote", on + "    allow_remote: true\n", "memory:\n  semantic:\n    allow_remote: false\n",
			SemanticRules{Enabled: true, Model: "nomic-embed-text", Dim: 768}},
	}
	for _, c := range cases {
		p := Merge(parseOrgForTest(t, c.org), parseForTest(t, c.project))
		if p.Err != nil {
			t.Fatalf("%s: %v", c.name, p.Err)
		}
		if got := p.MemoryRules().Semantic; got != c.want {
			t.Errorf("%s: Semantic = %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := (*Policy)(nil).MemoryRules().Semantic; got.Enabled {
		t.Errorf("no policy: semantic on, want off")
	}
}

func TestMemorySemanticRejectsBadSpecs(t *testing.T) {
	for name, doc := range map[string]string{
		"no-model":    "memory:\n  semantic:\n    enabled: true\n    dim: 8\n",
		"no-dim":      "memory:\n  semantic:\n    enabled: true\n    model: m\n",
		"huge-dim":    "memory:\n  semantic:\n    enabled: true\n    model: m\n    dim: 100000\n",
		"unknown-key": "memory:\n  semantic:\n    enabled: true\n    model: m\n    dim: 8\n    colour: blue\n",
	} {
		if p := parseForTest(t, doc); p.Err == nil || !strings.Contains(p.Err.Error(), "memory") {
			t.Errorf("%s: err = %v, want a memory parse error", name, p.Err)
		}
	}
}

// parseForTest parses a project document; "" is an empty policy and a parse
// error comes back as Err, as Load reports it.
func parseForTest(t *testing.T, doc string) *Policy {
	t.Helper()
	if doc == "" {
		return Empty()
	}
	p, err := parse([]byte(doc), "project")
	if err != nil {
		e := Empty()
		e.Err = err
		return e
	}
	return p
}

// parseOrgForTest is parseForTest for the org side; "" means no org file.
func parseOrgForTest(t *testing.T, doc string) *Policy {
	t.Helper()
	if doc == "" {
		return nil
	}
	return parseForTest(t, doc)
}

// Design 010 §3 S5: auto-recall is a second opt-in on top of semantic, off by
// default; the byte cap's lower value wins; either file can turn it off.
func TestMemoryAutoRecallRules(t *testing.T) {
	on := "memory:\n  semantic:\n    enabled: true\n    model: m\n    dim: 8\n"
	cases := []struct {
		name, project, org string
		auto               bool
		bytes              int
	}{
		{"semantic-on-auto-off", on, "", false, 0},
		{"auto-on-default-cap", on + "    auto_recall: true\n", "", true, DefaultAutoRecallBytes},
		{"cap-lower-wins", on + "    auto_recall: true\n    auto_recall_bytes: 4000\n", "memory:\n  semantic:\n    auto_recall_bytes: 600\n", true, 600},
		{"org-turns-auto-off", on + "    auto_recall: true\n", "memory:\n  semantic:\n    auto_recall: false\n", false, 0},
		{"auto-without-semantic", "memory:\n  semantic:\n    auto_recall: true\n", "", false, 0},
	}
	for _, c := range cases {
		p := Merge(parseOrgForTest(t, c.org), parseForTest(t, c.project))
		if p.Err != nil {
			t.Fatalf("%s: %v", c.name, p.Err)
		}
		r := p.MemoryRules().Semantic
		if r.AutoRecall != c.auto || r.AutoRecallBytes != c.bytes {
			t.Errorf("%s: AutoRecall=%v bytes=%d, want %v/%d", c.name, r.AutoRecall, r.AutoRecallBytes, c.auto, c.bytes)
		}
	}
	if p := parseForTest(t, on+"    auto_recall_bytes: 999999\n"); p.Err == nil {
		t.Errorf("huge auto_recall_bytes accepted")
	}
}
