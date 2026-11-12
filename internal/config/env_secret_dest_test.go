// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// EnvSecretMayGoTo: an environment secret goes only to a first-party server
// or the one RYSH_UPSTREAM_URL names (cli-core finding on rysh run --hosted).
func TestEnvSecretMayGoTo(t *testing.T) {
	t.Setenv("RYSH_UPSTREAM_URL", "")
	for u, want := range map[string]bool{
		"https://rysh.ai": true, "https://rysh.ai/": true, "https://RYSH.ai": true, "https://letchat.ai": true,
		"https://dev.rysh.ai": false, "https://evil.example": false, "http://rysh.ai": false, "": false,
	} {
		if got := EnvSecretMayGoTo(u); got != want {
			t.Errorf("%q: %v, want %v", u, got, want)
		}
	}
	t.Setenv("RYSH_UPSTREAM_URL", "https://dev.rysh.ai")
	if !EnvSecretMayGoTo("https://dev.rysh.ai/") || EnvSecretMayGoTo("https://evil.example") {
		t.Fatal("RYSH_UPSTREAM_URL does not decide")
	}
}
