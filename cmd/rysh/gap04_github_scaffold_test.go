// SPDX-License-Identifier: Apache-2.0

package main

// gap04 (root-order-074, G16; lead ruling R8 on Q6): github is not a
// `rysh assistant --channel` preset in v1 — that preset is E33's, later. The
// `github:` block is added to a profile by hand (rysh-cli
// docs/github-mentions.md), so no scaffold fields exist for it.

import "testing"

func TestGitHubHasNoScaffoldFields(t *testing.T) {
	if fields := channelCredFields("github"); len(fields) != 0 {
		t.Fatalf("github offers %d scaffold fields; the assistant preset is E33's (R8)", len(fields))
	}
}

// With no scaffold fields, github must not be offered by the `rysh setup` /
// onboard channel picker either: picking it would write a block with no token,
// secret or repositories. It stays a valid built-in type.
func TestGitHubIsNotOfferedByOnboardPicker(t *testing.T) {
	if !unofferedChannelTypes["github"] {
		t.Error("github is not in unofferedChannelTypes")
	}
	for _, ch := range onboardChannelTypes() {
		if ch == "github" {
			t.Fatal("the onboard channel picker offers github (R8: added by hand)")
		}
	}
}
