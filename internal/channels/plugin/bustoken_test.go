// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"path/filepath"
	"strings"
	"testing"
)

// gap01 slice 3: a nats-transport plugin is told WHERE the bus token is, never
// the token itself, and only for a loopback bus.
func TestPluginGetsTheTokenPathNotTheToken(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	env := pluginBusEnv("nats://127.0.0.1:4222")
	want := "RYSH_PLUGIN_NATS_TOKEN_FILE=" + filepath.Join(h, ".rysh-run", "nats-4222.token")
	if len(env) != 2 || env[0] != "RYSH_PLUGIN_NATS_URL=nats://127.0.0.1:4222" || env[1] != want {
		t.Fatalf("env = %q", env)
	}
	if env := pluginBusEnv("nats://remote.example:4222"); len(env) != 1 || strings.Contains(strings.Join(env, " "), "TOKEN") {
		t.Fatalf("non-loopback bus got a token path: %q", env)
	}
}
