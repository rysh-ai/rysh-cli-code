// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gap01 E4: sandbox.yaml's top-level default: and required: keys.

func writeSandboxYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadSettingsMissingFileIsOptIn(t *testing.T) {
	s, err := LoadSettings(t.TempDir())
	if err != nil || s.Default != "" || s.Required {
		t.Fatalf("settings = %+v err = %v", s, err)
	}
}

func TestLoadSettingsParses(t *testing.T) {
	dir := writeSandboxYAML(t, "default: offline\nrequired: true\nprofiles:\n  offline: {network: off}\n")
	s, err := LoadSettings(dir)
	if err != nil || s.Default != "offline" || !s.Required {
		t.Fatalf("settings = %+v err = %v", s, err)
	}
	// The profiles still load next to the new keys.
	all, err := LoadProfiles(dir)
	if err != nil || all["offline"].Network {
		t.Fatalf("profiles = %+v err = %v", all, err)
	}
}

func TestLoadSettingsBuiltinDefaultProfile(t *testing.T) {
	dir := writeSandboxYAML(t, "default: default\n")
	s, err := LoadSettings(dir)
	if err != nil || s.Default != DefaultProfile {
		t.Fatalf("settings = %+v err = %v", s, err)
	}
}

func TestLoadSettingsFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"unknown default":   "default: nope\n",
		"bad name":          "default: Bad Name\n",
		"required not bool": "required: maybe\n",
		"unknown key":       "requires: true\n",
		"garbage":           "{{{",
	} {
		if _, err := LoadSettings(writeSandboxYAML(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadSettingsRequiredWithoutDefault(t *testing.T) {
	s, err := LoadSettings(writeSandboxYAML(t, "required: true\n"))
	if err != nil || !s.Required || s.Default != "" {
		t.Fatalf("settings = %+v err = %v", s, err)
	}
	if !strings.Contains(s.String(), "required") {
		t.Fatalf("String() = %q", s.String())
	}
}
