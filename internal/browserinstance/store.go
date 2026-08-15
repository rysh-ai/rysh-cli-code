// SPDX-License-Identifier: Apache-2.0

// Package browserinstance manages persistent Chromium browser profiles for
// web-mode panes. Profiles live in the project-local
// <workDir>/.rysh/browser-instances/ directory (sibling to rysh.config.yaml),
// alongside .rysh/mcp.json and .rysh/forge/. Unlike the rest of .rysh, this
// directory is exempt from session-delete cleanup so logged-in browser
// sessions survive workspace/session deletion.
//
// rysh-cli does not render a browser; it only owns the on-disk profile
// directory and a small registry. The desktop app (rysh-cli-app) points its
// Chromium session storage at <workDir>/.rysh/browser-instances/ and uses the
// profile name as a persistent partition.
package browserinstance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultProfile is the profile name used when none is given to ##mode new web.
const DefaultProfile = "default"

// Profile is a persisted browser-profile record (the on-disk session data lives
// in the sibling directory of the same name).
type Profile struct {
	Name      string `json:"name"`
	CreatedMs int64  `json:"created_ms"`
}

// RootDir returns the browser-instances root for a project rooted at workDir:
// <workDir>/.rysh/browser-instances.
func RootDir(workDir string) string {
	return filepath.Join(workDir, ".rysh", "browser-instances")
}

// ProfileDir returns the on-disk directory for a single (sanitized) profile.
func ProfileDir(workDir, profile string) string {
	return filepath.Join(RootDir(workDir), SanitizeProfile(profile))
}

func registryPath(workDir string) string {
	return filepath.Join(RootDir(workDir), "registry.json")
}

// SanitizeProfile reduces a user-supplied profile name to a single safe path
// segment: path separators, "..", and surrounding dots/spaces are stripped so
// a profile can never escape RootDir. An empty result falls back to
// DefaultProfile.
func SanitizeProfile(name string) string {
	name = strings.TrimSpace(name)
	// Collapse any path structure to its last element and drop separators.
	name = filepath.Base(filepath.Clean(name))
	name = strings.ReplaceAll(name, string(filepath.Separator), "")
	name = strings.ReplaceAll(name, "/", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.Trim(name, ". ")
	if name == "" || name == "." || name == ".." {
		return DefaultProfile
	}
	return name
}

// EnsureProfile creates <workDir>/.rysh/browser-instances/<profile>/ (if absent)
// and records it in the registry. It returns the absolute profile directory.
func EnsureProfile(workDir, profile string) (string, error) {
	profile = SanitizeProfile(profile)
	dir := ProfileDir(workDir, profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create browser profile dir: %w", err)
	}

	regs, err := LoadRegistry(workDir)
	if err != nil {
		// A corrupt/unreadable registry should not block profile creation; the
		// directory is the source of truth. Start a fresh registry.
		regs = nil
	}
	found := false
	for _, p := range regs {
		if p.Name == profile {
			found = true
			break
		}
	}
	if !found {
		regs = append(regs, Profile{Name: profile, CreatedMs: time.Now().UnixMilli()})
		if err := saveRegistry(workDir, regs); err != nil {
			return "", err
		}
	}

	if abs, err := filepath.Abs(dir); err == nil {
		return abs, nil
	}
	return dir, nil
}

// LoadRegistry reads the known-profiles registry. A missing file yields an empty
// slice with no error.
func LoadRegistry(workDir string) ([]Profile, error) {
	data, err := os.ReadFile(registryPath(workDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var regs []Profile
	if err := json.Unmarshal(data, &regs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", registryPath(workDir), err)
	}
	return regs, nil
}

// saveRegistry writes the registry atomically (temp file + rename).
func saveRegistry(workDir string, regs []Profile) error {
	dir := RootDir(workDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if regs == nil {
		regs = []Profile{}
	}
	data, err := json.MarshalIndent(regs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "registry-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, registryPath(workDir))
}
