// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/importer"
)

// importTestEnv runs `rysh import` against a temp project with a fake tmux
// and a synthetic home. No test here runs the real tmux binary or reads the
// real home directory.
func importTestEnv(t *testing.T) (root string, run func(args ...string) (string, error), tmuxCalls *int) {
	t.Helper()
	root = t.TempDir()
	live, err := os.ReadFile(filepath.Join("..", "..", "internal", "importer", "testdata", "tmux-list-panes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	deps := importDeps{
		home: "/fixture-home",
		tmux: func(args []string) (string, error) {
			calls++
			if strings.Join(args, " ") != "list-panes -a -F "+importer.TmuxListPanesFormat {
				return "", errors.New("unexpected tmux args")
			}
			return string(live), nil
		},
	}
	run = func(args ...string) (string, error) {
		var out bytes.Buffer
		err := runImport(append([]string{"--dir", root}, args...), &out, deps)
		return out.String(), err
	}
	return root, run, &calls
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func TestImportTmuxPreviewWritesNothing(t *testing.T) {
	root, run, calls := importTestEnv(t)
	out, err := run("tmux")
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("tmux list-panes called %d times, want 1", *calls)
	}
	for _, want := range []string{"preview", `tab 1 "api:editor"`, "start: tail -f notes.log | grep TODO", ".rysh/imports/tmux.rysh"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if files := listFiles(t, root); len(files) != 0 {
		t.Errorf("preview wrote %q", files)
	}
}

func TestImportJSON(t *testing.T) {
	_, run, _ := importTestEnv(t)
	out, err := run("tmux", "--json", "--session", "api")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Source string
		Tabs   []struct{ Name string }
		Writes []string
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if doc.Source != "tmux" || len(doc.Tabs) != 2 || doc.Writes[0] != ".rysh/imports/tmux-api.rysh" {
		t.Errorf("json = %+v", doc)
	}
}

func TestImportApplyAndUndo(t *testing.T) {
	root, run, _ := importTestEnv(t)
	cfgFile := filepath.Join(root, "zellij.kdl")
	if err := os.WriteFile(cfgFile, []byte(`layout { tab name="a" { pane command="htop"; }; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run("zellij", cfgFile, "--apply")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, ".rysh", "imports", "zellij-zellij.rysh")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("apply did not write the script: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), "htop") || !strings.Contains(out, "rysh script .rysh/imports/zellij-zellij.rysh") {
		t.Errorf("apply output/script:\n%s\n%s", out, data)
	}
	if _, err := run("--undo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Error("undo must remove the script the import created")
	}
	if _, err := run("--undo"); err == nil {
		t.Error("undo with nothing to undo must fail")
	}
}

func TestImportUsageErrors(t *testing.T) {
	_, run, calls := importTestEnv(t)
	for _, args := range [][]string{{}, {"nosuch"}, {"zellij"}, {"tmux", "a", "b"}, {"tmux", "--bogus"}} {
		if _, err := run(args...); err == nil {
			t.Errorf("rysh import %q: want an error", args)
		}
	}
	if *calls != 0 {
		t.Errorf("a usage error must not reach tmux (%d calls)", *calls)
	}
}

func TestImportTmuxFileDoesNotRunTmux(t *testing.T) {
	root, run, calls := importTestEnv(t)
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "importer", "testdata", "tmuxp-shop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "shop.yaml")
	if err := os.WriteFile(f, src, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run("tmux", f)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Error("importing a tmuxp file must not run tmux")
	}
	if !strings.Contains(out, `tab 1 "dev"`) {
		t.Errorf("preview:\n%s", out)
	}
}

func TestImportMCPApplyAndUndo(t *testing.T) {
	root, run, _ := importTestEnv(t)
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "importer", "testdata", "mcp-project.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run("mcp"); err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("no .mcp.json in the project must name the file it looked for, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, ".rysh", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := []byte("[\n  {\"name\": \"mine\", \"transport\": \"stdio\", \"command\": \"x\"}\n]\n")
	if err := os.WriteFile(store, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run("mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "MCP servers: 4") {
		t.Errorf("preview:\n%s", shown(out))
	}
	if got, _ := os.ReadFile(store); string(got) != string(orig) {
		t.Fatal("preview changed .rysh/mcp.json")
	}
	if _, err := run("mcp", "--apply"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(store)
	if !strings.Contains(string(got), `"mine"`) || !strings.Contains(string(got), `"tracker"`) || strings.Contains(string(got), "placeholder-value") {
		t.Errorf("store after apply:\n%s", shown(got))
	}
	if _, err := run("--undo"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(store); string(got) != string(orig) {
		t.Errorf("undo did not restore .rysh/mcp.json exactly:\n%s", shown(got))
	}
}

func TestImportWarpAndITerm2(t *testing.T) {
	root, run, calls := importTestEnv(t)
	td := filepath.Join("..", "..", "internal", "importer", "testdata")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"warp", filepath.Join(td, "warp-dev.yaml")}, `tab 2 "app"`},
		{[]string{"iterm2", filepath.Join(td, "iterm2-arrangement.plist")}, `tab 1 "editor"`},
		{[]string{"iterm2", filepath.Join(td, "iterm2-arrangement.plist"), "--arrangement", "Other"}, ".rysh/imports/iterm2-other.rysh"},
	} {
		out, err := run(c.args...)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%q preview lacks %q:\n%s", c.args, c.want, out)
		}
	}
	if _, err := run("warp"); err == nil {
		t.Error("warp without a file must be an error")
	}
	if _, err := run("tmux", "--arrangement", "x"); err == nil {
		t.Error("--arrangement applies to iterm2 only")
	}
	if _, err := run("warp", filepath.Join(td, "warp-dev.yaml"), "--session", "x"); err == nil {
		t.Error("--session applies to live tmux only")
	}
	if *calls != 0 || len(listFiles(t, root)) != 0 {
		t.Error("a preview must not run tmux or write files")
	}
}

func TestImportClaudeApplyAndUndo(t *testing.T) {
	root, run, _ := importTestEnv(t)
	if _, err := run("claude"); err == nil || !strings.Contains(err.Error(), ".claude/settings.json") {
		t.Fatalf("no settings in the project must name the files it looked for, got %v", err)
	}
	td := filepath.Join("..", "..", "internal", "importer", "testdata")
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{"claude-settings.json": "settings.json", "claude-settings-local.json": "settings.local.json"} {
		data, err := os.ReadFile(filepath.Join(td, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".claude", dst), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pol := filepath.Join(root, ".rysh", "policy.yaml")
	if err := os.MkdirAll(filepath.Dir(pol), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := []byte("# mine\nbash:\n  deny: [rm]\n")
	if err := os.WriteFile(pol, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run("claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".claude/settings.json", ".claude/settings.local.json", `"curl*"`, "REVIEW comments only"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, shown(out))
		}
	}
	if strings.Contains(out, "placeholder-value") {
		t.Error("a settings env value reached the preview")
	}
	if _, err := run("claude", "--apply"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(pol)
	if !strings.Contains(string(got), "# mine") || !strings.Contains(string(got), `"curl*"`) {
		t.Errorf("policy after apply:\n%s", shown(got))
	}
	if _, err := run("--undo"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(pol); string(got) != string(orig) {
		t.Errorf("undo did not restore policy.yaml exactly:\n%s", shown(got))
	}
}

// shown is test output fit for a failure message. Output that carries one of
// the fixtures' synthetic secret values is withheld rather than printed: a
// failure message ends up in committed RED logs.
func shown[T string | []byte](text T) string {
	if strings.Contains(string(text), "placeholder-value") {
		return "(output withheld: it contains a fixture secret value)"
	}
	return string(text)
}
