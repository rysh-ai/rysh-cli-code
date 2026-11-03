// SPDX-License-Identifier: Apache-2.0

package config

// D-55 B: a project whose rysh.config.yaml names a NATS port of its own is
// marked OwnPort, so its daemon refuses to join another project's bus. The
// 24242 default, `port: 0` and a RYSH_NATS_PORT override are not "own".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNATSOwnPortOnlyFromTheFile(t *testing.T) {
	for _, tc := range []struct {
		name, nats, env string
		wantPort        int
		wantOwn         bool
	}{
		{"omitted keeps the shared default", "nats:\n  mode: embedded\n", "", 24242, false},
		{"explicit zero is private, not own", "nats:\n  port: 0\n", "", 0, false},
		{"explicit port is own", "nats:\n  port: 24310\n", "", 24310, true},
		{"RYSH_NATS_PORT overrides the file and is not own", "nats:\n  port: 24310\n", "26001", 26001, false},
		{"RYSH_NATS_PORT with no file port is not own", "nats:\n  mode: embedded\n", "26002", 26002, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RYSH_NATS_PORT", tc.env)
			path := filepath.Join(tempDir(t), "rysh.config.yaml")
			if err := os.WriteFile(path, []byte(tc.nats), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadFrom(path, new(strings.Builder))
			if err != nil {
				t.Fatalf("loadFrom: %v", err)
			}
			if cfg.NATS.Port != tc.wantPort || cfg.NATS.OwnPort != tc.wantOwn {
				t.Errorf("NATS.Port, OwnPort = %d, %v; want %d, %v", cfg.NATS.Port, cfg.NATS.OwnPort, tc.wantPort, tc.wantOwn)
			}
		})
	}
}
