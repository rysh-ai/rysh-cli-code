// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// fsRules compiles a project policy.yaml (and an optional org file) the way the
// daemon does: Merge(LoadOrg, Load) then ResolveFS against the rysh dir's parent.
func fsRules(t *testing.T, ryshDir, project, org string) *policy.FSRules {
	t.Helper()
	writeFile(t, filepath.Join(ryshDir, policy.FileName), project)
	var o *policy.Policy
	if org != "" {
		path := filepath.Join(t.TempDir(), "org.yaml")
		writeFile(t, path, org)
		o = policy.LoadOrg(path)
	}
	p := policy.Merge(o, policy.Load(ryshDir))
	if p.Err != nil {
		t.Fatalf("policy: %v", p.Err)
	}
	return p.ResolveFS(filepath.Dir(ryshDir))
}

func TestRulesFromFSNoRulesIsEmpty(t *testing.T) {
	f := RulesFromFS(nil)
	if f.FailClosed || len(f.ReadOnly)+len(f.ReadWrite)+len(f.Deny)+len(f.Unenforceable) != 0 {
		t.Fatalf("got %+v", f)
	}
}

func TestRulesFromFSFailClosed(t *testing.T) {
	if f := RulesFromFS(policy.FailClosedFS("x")); !f.FailClosed {
		t.Fatalf("got %+v", f)
	}
}

func TestRulesFromFSLiteralAndWildcards(t *testing.T) {
	l := newLayout(t)
	r := fsRules(t, l.rysh, `fs:
  read_only: ["**", "vendor/*.go"]
  read_write: ["src/**", "out", "gen/*"]
  deny: [".env", "**/secrets.txt"]
`, "")
	f := RulesFromFS(r)
	if want := []string{l.ws}; strings.Join(f.ReadOnly, "|") != strings.Join(want, "|") {
		t.Errorf("read_only = %v, want %v (\"**\" is the root itself)", f.ReadOnly, want)
	}
	if want := []string{filepath.Join(l.ws, "src"), filepath.Join(l.ws, "out")}; strings.Join(f.ReadWrite, "|") != strings.Join(want, "|") {
		t.Errorf("read_write = %v, want %v (wildcard carve-out dropped)", f.ReadWrite, want)
	}
	if want := []string{filepath.Join(l.ws, ".env")}; strings.Join(f.Deny, "|") != strings.Join(want, "|") {
		t.Errorf("deny = %v, want %v", f.Deny, want)
	}
	if len(f.Unenforceable) != 2 {
		t.Fatalf("unenforceable = %v, want the wildcard deny and the wildcard read_only", f.Unenforceable)
	}
}

func TestRulesFromFSOrgReadOnlyBeatsProjectCarveOut(t *testing.T) {
	l := newLayout(t)
	r := fsRules(t, l.rysh, "fs:\n  read_write: [\"src\"]\n", "fs:\n  read_only: [\"**\"]\n")
	f := RulesFromFS(r)
	if len(f.ReadWrite) != 0 {
		t.Fatalf("read_write = %v: a project carve-out out of an org read_only must not reach the sandbox", f.ReadWrite)
	}
	if len(f.ReadOnly) != 1 || f.ReadOnly[0] != l.ws {
		t.Fatalf("read_only = %v", f.ReadOnly)
	}
}

func TestRulesRoundTrip(t *testing.T) {
	l := newLayout(t)
	in := RulesFile{Version: 1, ReadOnly: []string{l.ws}, ReadWrite: []string{filepath.Join(l.ws, "src")}, Deny: []string{filepath.Join(l.ws, ".env")}}
	if err := WriteRules(l.rysh, in); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(RulesPath(l.rysh))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := LoadRules(l.rysh)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.ReadOnly, "|") != l.ws || len(got.ReadWrite) != 1 || len(got.Deny) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadRulesFailsClosed(t *testing.T) {
	l := newLayout(t)
	if _, err := LoadRules(l.rysh); err == nil || !strings.Contains(err.Error(), "never published") {
		t.Fatalf("missing file: err = %v", err)
	}
	cases := map[string]string{
		"fail closed":   `{"version":1,"fail_closed":true,"read_only":[],"read_write":[],"deny":[]}`,
		"unenforceable": `{"version":1,"read_only":[],"read_write":[],"deny":[],"unenforceable":["fs.deny **/.env"]}`,
		"unknown field": `{"version":1,"read_only":[],"read_write":[],"deny":[],"allow_all":true}`,
		"wrong version": `{"version":2,"read_only":[],"read_write":[],"deny":[]}`,
		"relative path": `{"version":1,"read_only":["src"],"read_write":[],"deny":[]}`,
		"wildcard path": `{"version":1,"read_only":[],"read_write":[],"deny":["/x/*"]}`,
		"not json":      `fs: {}`,
	}
	for name, body := range cases {
		writeFile(t, RulesPath(l.rysh), body)
		if _, err := LoadRules(l.rysh); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWriteRulesFailureRemovesOldFile(t *testing.T) {
	l := newLayout(t)
	if err := WriteRules(l.rysh, RulesFile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	renameFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFile = os.Rename })
	if err := WriteRules(l.rysh, RulesFile{Version: 1, ReadOnly: []string{l.ws}}); err == nil {
		t.Fatal("failed publish reported success")
	}
	if _, err := os.Stat(RulesPath(l.rysh)); !os.IsNotExist(err) {
		t.Fatalf("the old rules file survived a failed publish (stat err %v)", err)
	}
	if _, err := LoadRules(l.rysh); err == nil {
		t.Fatal("LoadRules accepted after a failed publish")
	}
}

func TestLoadRulesAddsResolvedForms(t *testing.T) {
	l := newLayout(t)
	link := filepath.Join(l.root, "link")
	if err := os.Symlink(l.ws, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteRules(l.rysh, RulesFile{Version: 1, ReadOnly: []string{}, ReadWrite: []string{}, Deny: []string{filepath.Join(link, "missing", ".env")}}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRules(l.rysh)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(l.ws, "missing", ".env")
	if !strings.Contains(strings.Join(got.Deny, "|"), want) {
		t.Fatalf("deny = %v, want the resolved %s too", got.Deny, want)
	}
}

func TestContainerPolicyReadOnlyWorkspaceWithCarveOut(t *testing.T) {
	l := newLayout(t)
	src := filepath.Join(l.ws, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := containerPlan(t, l, func(_ *Profile, s *Spec) {
		s.Rules = PathRules{ReadOnly: []string{l.ws}, ReadWrite: []string{src}}
	})
	argv := strings.Join(plan.Argv, " ")
	for _, want := range []string{
		"type=bind,source=" + l.ws + ",target=" + l.ws + ",readonly",
		"type=bind,source=" + src + ",target=" + src + " ",
		"type=bind,source=" + l.sbxHome + ",target=" + l.sbxHome + " ",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q", want)
		}
	}
	// The carve-out must come after (on top of) the read-only workspace.
	if strings.Index(argv, "target="+src+" ") < strings.Index(argv, "target="+l.ws+",readonly") {
		t.Error("carve-out mounted before the read-only workspace")
	}
}

func TestContainerPolicyRulesOnlyRestrict(t *testing.T) {
	l := newLayout(t)
	outside := filepath.Join(l.home, ".gitconfig")
	writeFile(t, outside, "[user]")
	opt := filepath.Join(l.root, "opt")
	if err := os.MkdirAll(opt, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := containerPlan(t, l, func(_ *Profile, s *Spec) {
		s.Rules = PathRules{
			ReadOnly:  []string{outside},
			ReadWrite: []string{opt},
			Deny:      []string{filepath.Join(l.ws, "no-such-file"), filepath.Join(l.home, "other")},
		}
	})
	argv := strings.Join(plan.Argv, " ")
	for _, never := range []string{outside, opt, "no-such-file", filepath.Join(l.home, "other")} {
		if strings.Contains(argv, never) {
			t.Errorf("argv mounts %s; a policy rule must never add a mount outside the workspace or for a missing path", never)
		}
	}
}

func TestContainerPolicyMissingReadOnlyRefuses(t *testing.T) {
	l := newLayout(t)
	p := builtinDefault()
	p.Backend, p.Image = BackendContainer, "local/agent:1"
	s := l.spec(p)
	s.Rules = PathRules{ReadOnly: []string{filepath.Join(l.ws, "dist")}}
	if _, err := (&Container{LookPath: fakeLook("docker")}).Prepare(s); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
	// Under a read-only parent the agent cannot create it: fine.
	s.Rules = PathRules{ReadOnly: []string{l.ws, filepath.Join(l.ws, "dist")}}
	if _, err := (&Container{LookPath: fakeLook("docker")}).Prepare(s); err != nil {
		t.Fatalf("under a read-only parent: %v", err)
	}
}

func TestCheckSpecRefusesDeniedWorkspace(t *testing.T) {
	l := newLayout(t)
	s := l.spec(builtinDefault())
	s.Rules = PathRules{Deny: []string{filepath.Dir(l.ws)}}
	if err := CheckSpec(s); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestMacOSPolicyLayering(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	src := filepath.Join(l.ws, "src")
	gen := filepath.Join(src, "gen")
	elsewhere := filepath.Join(l.root, "elsewhere")
	gitconfig := filepath.Join(l.home, ".gitconfig")
	plan, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) {
		s.Rules = PathRules{ReadOnly: []string{l.ws, gen, gitconfig}, ReadWrite: []string{src, elsewhere}}
	})
	if err != nil {
		t.Fatal(err)
	}
	ro := `(deny file-write* (subpath "` + l.ws + `"))`
	rw := `(allow file-write* (subpath "` + src + `"))`
	ro2 := `(deny file-write* (subpath "` + gen + `"))`
	i, j, k := strings.Index(plan.SBPL, ro), strings.Index(plan.SBPL, rw), strings.Index(plan.SBPL, ro2)
	if i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Fatalf("want %s, then %s, then %s in order; profile:\n%s", ro, rw, ro2, plan.SBPL)
	}
	if strings.Contains(plan.SBPL, `(allow file-write* (subpath "`+elsewhere+`"))`) {
		t.Error("a read_write rule outside the workspace opened writes")
	}
	for _, line := range strings.Split(plan.SBPL, "\n") {
		if strings.HasPrefix(line, "(allow") && strings.Contains(line, gitconfig) {
			t.Errorf("a policy read_only rule under $HOME granted access: %s", line)
		}
	}
	// The rysh dir deny and the sandbox home re-allow still come after.
	if h := strings.Index(plan.SBPL, `(allow file-read* file-read-data file-write* (subpath "`+l.sbxHome+`"))`); h < k {
		t.Error("the sandbox home re-allow must come after the policy rules")
	}
}
