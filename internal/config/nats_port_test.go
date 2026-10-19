// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNATSPortZeroIsHonoured pins the difference between `port: 0` and no port
// key. The daemon tests write `port: 0` to get a kernel-assigned port; when it
// was read as "unset" every test daemon got 24242 and joined the developer's
// live NATS, leaving about ten buckets behind in that store per test session.
func TestNATSPortZeroIsHonoured(t *testing.T) {
	t.Setenv("RYSH_NATS_PORT", "")
	for _, tc := range []struct {
		name, nats string
		want       int
	}{
		{"omitted keeps the default", "nats:\n  mode: embedded\n", 24242},
		{"explicit zero means kernel-assigned", "nats:\n  port: 0\n", 0},
		{"explicit port", "nats:\n  port: 25000\n", 25000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tempDir(t), "rysh.config.yaml")
			if err := os.WriteFile(path, []byte(tc.nats), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadFrom(path, new(strings.Builder))
			if err != nil {
				t.Fatalf("loadFrom: %v", err)
			}
			if cfg.NATS.Port != tc.want {
				t.Errorf("NATS.Port = %d, want %d", cfg.NATS.Port, tc.want)
			}
		})
	}
}
