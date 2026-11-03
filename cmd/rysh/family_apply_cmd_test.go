// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// `rysh heymates apply` takes no flag but --session: the tool allow-list and
// everything else come from the manifest and the command itself.
func TestFamilyApply_RefusesUnknownFlags(t *testing.T) {
	for _, args := range [][]string{{"--tools", "bash"}, {"--prompt-file", "x"}, {"extra"}, {"--session"}} {
		err := runFamilyApply(config.Config{}, args)
		if err == nil || !strings.Contains(err.Error(), "usage: rysh heymates apply") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestFamilyUsageNamesApply(t *testing.T) {
	if !strings.Contains(familyUsage, "rysh heymates apply") {
		t.Error("family usage does not list apply")
	}
}

// Every failing `rysh family apply` says "family apply" in its output:
// rysh-server's hosted runner (internal/hostedrunner/docker.go:540) takes that
// text as "this image has the command" and never reports such a failure as
// ErrApplyUnsupported. The usage now names `rysh heymates apply`, so the error
// prefix is the marker; keep it until phase 4 changes docker.go's matchers.
func TestFamilyApply_FailuresNameFamilyApply(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("{not a manifest")
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()

	err = runFamilyApply(config.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "family apply") {
		t.Fatalf("bad manifest: err = %v, want it to name 'family apply'", err)
	}
	if err := runFamilyApply(config.Config{}, []string{"--tools", "bash"}); err == nil || !strings.Contains(err.Error(), "family apply") {
		t.Fatalf("unknown flag: err = %v, want it to name 'family apply'", err)
	}
}
