// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteHostsLoadedFromConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(p, []byte(`remote:
  hosts:
    - alias: rysh-agent-fleet-dev
      session: dev
      dir: /home/u/ws
    - alias: m5
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromFile(p, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RemoteHosts) != 2 || cfg.RemoteHosts[0].Alias != "rysh-agent-fleet-dev" ||
		cfg.RemoteHosts[0].Session != "dev" || cfg.RemoteHosts[0].Dir != "/home/u/ws" || cfg.RemoteHosts[1].Alias != "m5" {
		t.Fatalf("hosts = %+v", cfg.RemoteHosts)
	}
}
