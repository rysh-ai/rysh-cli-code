// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"regexp"
	"testing"
)

var e2eRunRE = regexp.MustCompile(`^[a-z0-9]{1,12}$`)

// e2eID is the Spec.ID a Docker sandbox E2E test uses. With
// RYSH_SANDBOX_E2E_RUN=<run> (the dev-host runner's docker-sandbox part) it
// is e2e<run>-<id>, so the container is rysh-sbx-e2e<run>-…, and the runner
// removes only names with its own run's prefix, never a blanket filter: this
// Mac's Docker hosts other work (security-lead D1c). The id's tail, where its
// random part is, is kept when the whole would pass ValidName's 32 bytes.
// Unset, the id is unchanged.
func e2eID(t *testing.T, id string) string {
	t.Helper()
	run := os.Getenv("RYSH_SANDBOX_E2E_RUN")
	if run == "" {
		return id
	}
	if !e2eRunRE.MatchString(run) {
		t.Fatalf("RYSH_SANDBOX_E2E_RUN=%q: want [a-z0-9]{1,12}", run)
	}
	prefix := "e2e" + run + "-"
	if room := 32 - len(prefix); len(id) > room {
		id = id[len(id)-room:]
	}
	return prefix + id
}

func TestE2EIDCarriesTheRunPrefix(t *testing.T) {
	t.Setenv("RYSH_SANDBOX_E2E_RUN", "")
	if got := e2eID(t, "g1bustok"); got != "g1bustok" {
		t.Fatalf("unset run: %q, want the id unchanged", got)
	}
	t.Setenv("RYSH_SANDBOX_E2E_RUN", "r20261106ab")
	for _, id := range []string{"g1bustok", "e2epolabcdefghijklmnopqrstuvwxyz0123"} {
		got := e2eID(t, id)
		if !ValidName(got) || got[:len("e2er20261106ab-")] != "e2er20261106ab-" {
			t.Fatalf("e2eID(%q) = %q: want a valid id starting e2er20261106ab-", id, got)
		}
		if got[len(got)-4:] != id[len(id)-4:] {
			t.Fatalf("e2eID(%q) = %q: the random tail was cut", id, got)
		}
	}
}
