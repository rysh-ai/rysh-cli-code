package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// proxy.strict (design 022 §8.2) escalates the ungoverned-CLI warning to a
// block. It is a separate switch from proxy.required on purpose — see the
// comment on Policy.ProxyStrict — and, like every other merged rule, an org
// file that sets it cannot be softened by a project file that omits it.

// writePolicy writes a policy file into ryshDir (what Load takes).
func writePolicy(t *testing.T, ryshDir, body string) string {
	t.Helper()
	if err := os.MkdirAll(ryshDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(ryshDir, FileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func TestPolicy_ProxyStrictFromFile(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "proxy:\n  required: true\n  strict: true\n")

	p := Load(dir)
	if p.Err != nil {
		t.Fatalf("load: %v", p.Err)
	}
	if !p.ProxyStrict {
		t.Fatal("proxy.strict did not reach the policy")
	}
	if !strings.Contains(p.Summary(), "strict") {
		t.Errorf("##policy must show strict mode — a rule that stops programs "+
			"cannot be invisible:\n%s", p.Summary())
	}
}

func TestPolicy_ProxyStrictDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "proxy:\n  required: true\n")

	p := Load(dir)
	if p.ProxyStrict {
		t.Fatal("proxy.required must not imply strict — 022 §8.2 is explicit " +
			"that acting on negative evidence is a separate decision")
	}
}

func TestMerge_ProxyStrictIsStrictestWins(t *testing.T) {
	cases := []struct {
		org, project, want bool
	}{
		{false, false, false},
		{true, false, true}, // a project cannot soften the org by omission
		{false, true, true},
		{true, true, true},
	}
	for _, c := range cases {
		m := Merge(
			&Policy{ProxyStrict: c.org, Loaded: true},
			&Policy{ProxyStrict: c.project, Loaded: true},
		)
		if m.ProxyStrict != c.want {
			t.Errorf("Merge(org=%v, project=%v).ProxyStrict = %v, want %v",
				c.org, c.project, m.ProxyStrict, c.want)
		}
	}
}
