// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMemoryPolicy(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMemoryDefaultsWithNoPolicyFile(t *testing.T) {
	m := Load(t.TempDir()).MemoryRules()
	if !m.Enabled || !m.Inject || m.MaxInjectBytes != DefaultMemoryInjectBytes || m.Reason != "" {
		t.Fatalf("defaults = %+v", m)
	}
	var nilPolicy *Policy
	if m := nilPolicy.MemoryRules(); !m.Enabled || !m.Inject {
		t.Fatalf("nil policy (no policy loaded yet) = %+v, want the defaults", m)
	}
}

func TestMemorySectionParses(t *testing.T) {
	p := Load(writeMemoryPolicy(t, "memory:\n  inject: false\n  max_inject_bytes: 2048\n"))
	if p.Err != nil {
		t.Fatalf("Err = %v", p.Err)
	}
	m := p.MemoryRules()
	if !m.Enabled || m.Inject || m.MaxInjectBytes != 2048 {
		t.Fatalf("rules = %+v", m)
	}
	if !strings.Contains(p.Summary(), "memory:") {
		t.Fatalf("##policy summary does not show the memory section")
	}
}

func TestMemorySectionRejectsUnknownKeysAndBadCaps(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key":  "memory:\n  enabled: true\n  share: true\n",
		"zero cap":     "memory:\n  max_inject_bytes: 0\n",
		"negative cap": "memory:\n  max_inject_bytes: -5\n",
		"huge cap":     "memory:\n  max_inject_bytes: 1000000\n",
		"not a map":    "memory: yes\n",
	} {
		p := Load(writeMemoryPolicy(t, doc))
		if p.Err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		m := p.MemoryRules()
		if m.Enabled || m.Inject || m.Reason == "" {
			t.Errorf("%s: a broken policy must turn memory off with a reason, got %+v", name, m)
		}
	}
}

func TestMemoryFailsClosedOnAnyPolicyError(t *testing.T) {
	p := Empty()
	p.Err = errors.New("boom")
	if m := p.MemoryRules(); m.Enabled || m.Inject || m.Reason == "" {
		t.Fatalf("errored policy = %+v", m)
	}
}

func TestMemoryMergeStrictestWins(t *testing.T) {
	load := func(doc string) *Policy {
		p, err := parse([]byte(doc), "x")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, org, proj string
		enabled, inject bool
		max             int
	}{
		{"org disables", "memory:\n  enabled: false\n", "memory:\n  enabled: true\n", false, false, DefaultMemoryInjectBytes},
		{"project disables", "memory:\n  enabled: true\n", "memory:\n  enabled: false\n", false, false, DefaultMemoryInjectBytes},
		{"org no inject", "memory:\n  inject: false\n", "", true, false, DefaultMemoryInjectBytes},
		{"lower cap wins (org)", "memory:\n  max_inject_bytes: 1000\n", "memory:\n  max_inject_bytes: 3000\n", true, true, 1000},
		{"lower cap wins (project)", "memory:\n  max_inject_bytes: 3000\n", "memory:\n  max_inject_bytes: 500\n", true, true, 500},
		{"project cannot re-enable", "memory:\n  enabled: false\n", "", false, false, DefaultMemoryInjectBytes},
	}
	for _, c := range cases {
		m := Merge(load(c.org), load(c.proj)).MemoryRules()
		if m.Enabled != c.enabled || m.Inject != c.inject || m.MaxInjectBytes != c.max {
			t.Errorf("%s: got %+v", c.name, m)
		}
	}
}
