// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/script"
)

// fixtureHome stands in for $HOME in every fixture. It is synthetic on
// purpose: no test reads the real home directory or names a real user.
const fixtureHome = "/fixture-home"

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// golden compares got with testdata/<name>, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update to create it)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// shape flattens a plan into "tab: lane1[cwd|cmd, ...] lane2[...]" lines so a
// test states the expected layout in one readable literal.
func shape(p *Plan) []string {
	var out []string
	for _, tab := range p.Tabs {
		var lanes []string
		for _, lane := range tab.Lanes {
			var panes []string
			for _, pane := range lane.Panes {
				panes = append(panes, pane.Cwd+"|"+pane.Command)
			}
			lanes = append(lanes, "["+strings.Join(panes, ", ")+"]")
		}
		out = append(out, tab.Name+": "+strings.Join(lanes, " "))
	}
	return out
}

func assertShape(t *testing.T, p *Plan, want []string) {
	t.Helper()
	if got := shape(p); !reflect.DeepEqual(got, want) {
		t.Errorf("layout shape:\n got  %q\n want %q", got, want)
	}
}

// --- tmux layout strings ---------------------------------------------------

func TestParseTmuxLayout_NestedSplits(t *testing.T) {
	n, err := ParseTmuxLayout("b25d,238x58,0,0{119x58,0,0,1,118x58,120,0[118x29,120,0,2,118x28,120,30,3]}")
	if err != nil {
		t.Fatal(err)
	}
	if n.Split != SplitSideBySide || len(n.Children) != 2 {
		t.Fatalf("root = %+v, want a side-by-side split of 2", n)
	}
	if n.Children[0].PaneID != "1" || n.Children[1].Split != SplitStacked {
		t.Fatalf("children = %+v", n.Children)
	}
	if got := n.Children[1].Children[1].PaneID; got != "3" {
		t.Errorf("deep leaf pane id = %q, want 3", got)
	}
}

func TestParseTmuxLayout_SinglePaneAndErrors(t *testing.T) {
	n, err := ParseTmuxLayout("a1b2,238x58,0,0,4")
	if err != nil || n.PaneID != "4" || n.Split != SplitNone {
		t.Fatalf("single pane: %+v, %v", n, err)
	}
	for _, bad := range []string{"", "zz", "b25d,238x58,0,0{119x58,0,0,1", "b25d,238x58,0,0{}"} {
		if _, err := ParseTmuxLayout(bad); err == nil {
			t.Errorf("ParseTmuxLayout(%q) = nil error, want an error", bad)
		}
	}
}

// --- tmux live -------------------------------------------------------------

func TestParseTmuxLive(t *testing.T) {
	p, err := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "")
	if err != nil {
		t.Fatal(err)
	}
	assertShape(t, p, []string{
		"api:editor: [$HOME/src/api|vim .] [$HOME/src/api|, $HOME/src/api/test|go test ./...]",
		"api:server: [$HOME/src/api|make run]",
		"notes:main: [/srv/notes|, /srv/notes|tail -f notes.log | grep TODO, /srv/notes/my drafts|]",
	})
	if !hasNote(p, "flattened") {
		t.Errorf("the nested split in notes:main must be reported as flattened; notes = %q", p.Notes)
	}
}

func TestParseTmuxLive_SessionFilter(t *testing.T) {
	p, err := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "notes")
	if err != nil {
		t.Fatal(err)
	}
	// One session selected: tabs are named by window only.
	assertShape(t, p, []string{
		"main: [/srv/notes|, /srv/notes|tail -f notes.log | grep TODO, /srv/notes/my drafts|]",
	})
	if _, err := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "nope"); err == nil {
		t.Error("an unknown --session must be an error, not an empty plan")
	}
}

func TestParseTmuxLive_Malformed(t *testing.T) {
	if _, err := ParseTmuxLive("api\t1\teditor\n", fixtureHome, ""); err == nil {
		t.Error("a short line must be an error")
	}
	if _, err := ParseTmuxLive("", fixtureHome, ""); err == nil {
		t.Error("no panes at all must be an error (is tmux running?)")
	}
}

// --- tmuxinator / tmuxp ----------------------------------------------------

func TestParseTmuxinator(t *testing.T) {
	p, err := ParseTmuxConfig(readFixture(t, "tmuxinator-blog.yml"), fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "blog" {
		t.Errorf("name = %q, want blog", p.Name)
	}
	assertShape(t, p, []string{
		"editor: [$HOME/code/blog|nvm use 20; vim] [$HOME/code/blog|nvm use 20; guard, $HOME/code/blog|nvm use 20; cd log; tail -f development.log]",
		"server: [$HOME/code/blog|nvm use 20; bundle exec rails s]",
		"deploy: [$HOME/code/blog-infra|nvm use 20; terraform plan] [$HOME/code/blog-infra|nvm use 20]",
		"shell: [$HOME/code/blog|nvm use 20]",
	})
}

func TestParseTmuxp(t *testing.T) {
	p, err := ParseTmuxConfig(readFixture(t, "tmuxp-shop.yaml"), fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "shop" {
		t.Errorf("name = %q, want shop", p.Name)
	}
	act := "source .venv/bin/activate; "
	assertShape(t, p, []string{
		"dev: [$HOME/work/shop|" + act + "python manage.py runserver] [$HOME/work/shop|" + act + "npm run watch] [$HOME/work/shop|source .venv/bin/activate]",
		"db: [/srv/db/sql|" + act + "psql shop, /srv/db|source .venv/bin/activate]",
		"grid: [$HOME/work/shop|" + act + "htop, $HOME/work/shop|source .venv/bin/activate] [$HOME/work/shop|source .venv/bin/activate, $HOME/work/shop|source .venv/bin/activate]",
	})
	if !hasNote(p, "tiled") {
		t.Errorf("a tiled layout is an approximation and must say so; notes = %q", p.Notes)
	}
}

func TestParseTmuxConfig_Unknown(t *testing.T) {
	if _, err := ParseTmuxConfig([]byte("foo: bar\n"), fixtureHome); err == nil {
		t.Error("YAML that is neither tmuxinator nor tmuxp must be an error")
	}
	if _, err := ParseTmuxConfig([]byte("name: [\n"), fixtureHome); err == nil {
		t.Error("broken YAML must be an error")
	}
}

// --- Zellij ----------------------------------------------------------------

func TestParseKDL(t *testing.T) {
	nodes, err := ParseKDL([]byte(`a "x" k=1 { b r#"raw "q""#; /- c; d /- skipped e=true }
// line comment
f \
  "continued"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].Name != "a" || nodes[1].Name != "f" {
		t.Fatalf("top nodes = %+v", nodes)
	}
	a := nodes[0]
	if a.Args[0] != "x" || a.Props["k"] != "1" {
		t.Errorf("a args/props = %q %q", a.Args, a.Props)
	}
	if len(a.Children) != 2 || a.Children[0].Args[0] != `raw "q"` || a.Children[1].Name != "d" {
		t.Fatalf("a children = %+v", a.Children)
	}
	if d := a.Children[1]; len(d.Args) != 0 || d.Props["e"] != "true" {
		t.Errorf("slashdash arg not dropped: %+v", d)
	}
	if nodes[1].Args[0] != "continued" {
		t.Errorf("line continuation: %+v", nodes[1])
	}
	for _, bad := range []string{`a {`, `a "unterminated`, `a }`, `/* open`} {
		if _, err := ParseKDL([]byte(bad)); err == nil {
			t.Errorf("ParseKDL(%q) = nil error", bad)
		}
	}
}

func TestParseZellij(t *testing.T) {
	p, err := ParseZellij(readFixture(t, "zellij-web.kdl"), fixtureHome, "web")
	if err != nil {
		t.Fatal(err)
	}
	assertShape(t, p, []string{
		"code: [$HOME/src/web|nvim .] [$HOME/src/web/frontend|npm run dev, $HOME/src/web|]",
		"logs: [/var/log|tail -f 'system log.txt', /var/log|${EDITOR:-vi} notes.md]",
	})
	if p.Tabs[0].Lanes[1].Panes[1].Name != "scratch" {
		t.Errorf("pane name not kept: %+v", p.Tabs[0].Lanes[1].Panes[1])
	}
	if !hasNote(p, "plugin") {
		t.Errorf("a skipped plugin pane must be noted; notes = %q", p.Notes)
	}
}

func TestParseZellij_NoTabs(t *testing.T) {
	p, err := ParseZellij([]byte(`layout { pane command="htop"; pane; }`), fixtureHome, "solo")
	if err != nil {
		t.Fatal(err)
	}
	assertShape(t, p, []string{"solo: [|htop, |]"})
	if _, err := ParseZellij([]byte(`keybinds { }`), fixtureHome, "x"); err == nil {
		t.Error("a KDL file with no layout node must be an error")
	}
}

// --- preview rendering -----------------------------------------------------

func TestPreviewGolden(t *testing.T) {
	cases := []struct {
		name string
		plan func() (*Plan, error)
	}{
		{"tmux-live", func() (*Plan, error) {
			return ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "")
		}},
		{"tmuxinator", func() (*Plan, error) { return ParseTmuxConfig(readFixture(t, "tmuxinator-blog.yml"), fixtureHome) }},
		{"tmuxp", func() (*Plan, error) { return ParseTmuxConfig(readFixture(t, "tmuxp-shop.yaml"), fixtureHome) }},
		{"zellij", func() (*Plan, error) { return ParseZellij(readFixture(t, "zellij-web.kdl"), fixtureHome, "web") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := c.plan()
			if err != nil {
				t.Fatal(err)
			}
			p.Inputs = []string{"testdata/" + c.name}
			golden(t, c.name+".preview.txt", []byte(p.Text()))
			js, err := p.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(js) {
				t.Fatal("JSON() is not valid JSON")
			}
			golden(t, c.name+".preview.json", js)
			golden(t, c.name+".rysh", []byte(Script(p)))
		})
	}
}

func TestPreviewIsStable(t *testing.T) {
	a, _ := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "")
	b, _ := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "")
	if a.Text() != b.Text() {
		t.Error("two previews of the same input differ")
	}
}

// --- the generated .rysh script --------------------------------------------

// The script must survive `rysh script`'s transpiler, and every ## line in it
// must be one of the verbs the importer promises (the ones whose semantics the
// script relies on). A start command must never reach the transpiler as bash
// syntax: if it did, its && or | would run at script time in the script's own
// shell instead of in the pane.
func TestScriptTranspilesAndUsesOnlyKnownVerbs(t *testing.T) {
	p, err := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "")
	if err != nil {
		t.Fatal(err)
	}
	src := Script(p)
	res, err := script.Transpile(src)
	if err != nil {
		t.Fatalf("generated script does not transpile: %v", err)
	}
	lines := strings.Split(src, "\n")
	allowed := map[string]bool{}
	for _, v := range ScriptVerbs {
		allowed[v] = true
	}
	for _, n := range res.RyshLines {
		line := strings.TrimSpace(lines[n-1])
		verb := strings.Fields(strings.TrimPrefix(line, "##"))[0]
		if !allowed[verb] {
			t.Errorf("line %d uses ##%s, not in ScriptVerbs %v", n, verb, ScriptVerbs)
		}
		for _, op := range []string{"&&", "||", "|", ";", ">", "<", "&"} {
			if strings.Contains(line, op) {
				t.Errorf("line %d carries the shell operator %q on a ## line: %s", n, op, line)
			}
		}
	}
	if len(res.RyshLines) == 0 {
		t.Fatal("no ## lines generated")
	}
	if !strings.Contains(src, `'tail -f notes.log | grep TODO'`) && !strings.Contains(src, `tail -f notes.log | grep TODO`) {
		t.Error("the pipe start command is missing from the script")
	}
}

// Every value the script hands a pane shell is single-quoted in a plain bash
// assignment, so no character in a path or command can break out of it.
func TestScriptQuotesHostileText(t *testing.T) {
	p := &Plan{Source: "tmux", Tabs: []Tab{{Name: `it's "x" $(id)`, Lanes: []Lane{{Panes: []Pane{
		{Cwd: `/srv/a'b $(x)`, Command: "echo `id`; rm -rf \"$HOME\""},
	}}}}}}
	src := Script(p)
	if _, err := script.Transpile(src); err != nil {
		t.Fatalf("transpile: %v\n%s", err, src)
	}
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "##") && (strings.Contains(trim, "`") || strings.Contains(trim, "$(")) {
			t.Errorf("hostile text reached a ## line unquoted: %s", trim)
		}
	}
}

// --- context files ---------------------------------------------------------

func TestContextFilesListedNotCopied(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("# notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ContextFiles(root)
	if !reflect.DeepEqual(got, []string{"AGENTS.md", "CLAUDE.md"}) {
		t.Errorf("ContextFiles = %q", got)
	}
	p := &Plan{Source: "tmux", Context: got}
	if txt := p.Text(); !strings.Contains(txt, "CLAUDE.md") || !strings.Contains(txt, "left where they are") {
		t.Errorf("preview must list context files as left in place:\n%s", txt)
	}
	if ch := p.Changes(root); len(ch) != 0 {
		t.Errorf("context files must never become changes: %+v", ch)
	}
}

func hasNote(p *Plan, sub string) bool {
	for _, n := range p.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
