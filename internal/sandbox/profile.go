// SPDX-License-Identifier: Apache-2.0

// Package sandbox runs an agent pane's process isolated (design 066, feature
// #13). It is OPT-IN — `##claude --sandbox <profile>` — and FAIL-CLOSED: a pane
// that asks for a sandbox either gets exactly the one its profile describes or
// does not start. Nothing in this package ever falls back to running the agent
// unsandboxed.
//
// Two backends sit behind one interface: a container (docker, else podman, from
// PATH) and macOS sandbox-exec with a generated profile. Both see the same
// Spec — the workspace read-write at its own path, a dedicated per-profile home
// in place of $HOME, an env that has already been through agentenv.Filter — and
// both produce a Plan: the argv rysh exec()s in place of the agent, plus the
// mounts and env NAMES that argv exposes, which is what the tests list to prove
// no credential crosses.
package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the profile file, in the rysh directory next to policy.yaml.
const FileName = "sandbox.yaml"

// Backend names as written in a profile.
const (
	BackendAuto      = "auto"
	BackendContainer = "container"
	BackendMacOS     = "macos"
	BackendBwrap     = "bwrap"
)

// DefaultProfile is the built-in profile name. A sandbox.yaml may redefine it.
const DefaultProfile = "default"

// defaultCopyConfig is what a profile without copy_config copies from the host
// agent config dir into the sandbox home: instructions and commands, nothing
// that can hold a credential. settings.json is deliberately NOT here — its env
// block and apiKeyHelper can carry one — so copying it is the user's explicit
// choice.
var defaultCopyConfig = []string{"CLAUDE.md", "commands", "agents"}

// refusedConfigNames are never copied, whatever a profile says: the files the
// agent CLIs keep their credentials in.
var refusedConfigNames = []string{".credentials.json", ".claude.json", "auth.json", "credentials.json"}

var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Profile is one named sandbox shape.
type Profile struct {
	Name    string
	Backend string // auto | container | macos | bwrap
	// Image is the container image (container backend; required there). It is
	// run with --pull never: rysh never fetches an image.
	Image string
	// Network is true for `network: on` (the default, as today) and false for
	// `off` and `proxy`. Founder ruling #2: never a host allow-list.
	Network bool
	// Proxy is `network: proxy` (gap01 slice 2, R9): no general network; the
	// agent reaches its model through rysh's governance proxy only, and the
	// provider key stays on the host.
	Proxy bool
	// ReadOnly are extra absolute host paths made visible read-only.
	ReadOnly []string
	// CopyConfig are entries of the host agent config dir (~/.claude) copied
	// into the sandbox home on every launch.
	CopyConfig []string
}

type yamlProfile struct {
	Backend    string    `yaml:"backend"`
	Image      string    `yaml:"image"`
	Network    string    `yaml:"network"`
	ReadOnly   []string  `yaml:"read_only"`
	CopyConfig *[]string `yaml:"copy_config"`
}

type yamlFile struct {
	// Default and Required are the workspace's sandbox posture (gap01 E4).
	Default  string                 `yaml:"default"`
	Required bool                   `yaml:"required"`
	Profiles map[string]yamlProfile `yaml:"profiles"`
}

// Settings is the workspace's sandbox posture, from sandbox.yaml's top level.
// The file lives in the rysh dir, which every sandbox hides, so an agent can
// never relax the posture it runs under.
type Settings struct {
	// Default is the profile a `##claude` / `##codex` launch gets when it
	// names none ("" = opt-in, as before). `--no-sandbox` opts one launch out,
	// unless Required.
	Default string
	// Required refuses every agent launch in this workspace that would run
	// unsandboxed: no `--no-sandbox`, and no restore of an unsandboxed pane.
	Required bool
}

func (s Settings) String() string {
	d := s.Default
	if d == "" {
		d = "(none)"
	}
	return fmt.Sprintf("default=%s required=%v", d, s.Required)
}

// builtinDefault is `default` when sandbox.yaml does not define it.
func builtinDefault() Profile {
	return Profile{
		Name:       DefaultProfile,
		Backend:    BackendAuto,
		Network:    true,
		CopyConfig: append([]string(nil), defaultCopyConfig...),
	}
}

// ValidName reports whether s is a usable profile name. Names reach a shell
// line and a directory name, so the set is deliberately narrow.
func ValidName(s string) bool { return profileNameRE.MatchString(s) }

// LoadProfiles reads <ryshDir>/sandbox.yaml. A missing file is not an error: it
// yields the built-in default only. A file that exists but does not parse, or
// carries an unknown key or a bad value, IS an error — a typo in a sandbox
// profile must stop the pane, not quietly widen what it can reach.
func LoadProfiles(ryshDir string) (map[string]Profile, error) {
	all, _, err := load(ryshDir)
	return all, err
}

// LoadSettings reads the top-level posture of <ryshDir>/sandbox.yaml. A missing
// file is opt-in (no default, not required). A file that does not parse, or
// whose default names no profile, is an error: the caller cannot tell whether
// a sandbox is required, so it must refuse the launch.
func LoadSettings(ryshDir string) (Settings, error) {
	_, set, err := load(ryshDir)
	return set, err
}

func load(ryshDir string) (map[string]Profile, Settings, error) {
	out := map[string]Profile{DefaultProfile: builtinDefault()}
	if strings.TrimSpace(ryshDir) == "" {
		return out, Settings{}, nil
	}
	path := filepath.Join(ryshDir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, Settings{}, nil
	}
	if err != nil {
		return nil, Settings{}, fmt.Errorf("read %s: %w", path, err)
	}
	var f yamlFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) { // io.EOF: an empty file
		return nil, Settings{}, fmt.Errorf("%s: %w", path, err)
	}
	for name, yp := range f.Profiles {
		p, err := parseProfile(name, yp)
		if err != nil {
			return nil, Settings{}, fmt.Errorf("%s: profile %q: %w", path, name, err)
		}
		out[name] = p
	}
	set := Settings{Default: strings.TrimSpace(f.Default), Required: f.Required}
	if set.Default != "" {
		if !ValidName(set.Default) {
			return nil, Settings{}, fmt.Errorf("%s: default %q: not a valid profile name", path, set.Default)
		}
		if _, ok := out[set.Default]; !ok {
			return nil, Settings{}, fmt.Errorf("%s: default %q names no profile", path, set.Default)
		}
	}
	return out, set, nil
}

func parseProfile(name string, yp yamlProfile) (Profile, error) {
	if !ValidName(name) {
		return Profile{}, fmt.Errorf("name must match %s", profileNameRE)
	}
	p := Profile{Name: name, Network: true}
	switch b := strings.TrimSpace(yp.Backend); b {
	case "", BackendAuto:
		p.Backend = BackendAuto
	case BackendContainer, BackendMacOS, BackendBwrap:
		p.Backend = b
	default:
		return Profile{}, fmt.Errorf("backend %q: want auto, container, macos or bwrap", b)
	}
	p.Image = strings.TrimSpace(yp.Image)
	switch n := strings.TrimSpace(yp.Network); n {
	case "", "on":
		p.Network = true
	case "off":
		p.Network = false
	case "proxy":
		p.Network, p.Proxy = false, true
	default:
		return Profile{}, fmt.Errorf("network %q: want on, off or proxy (host allow-lists are not offered)", n)
	}
	for _, ro := range yp.ReadOnly {
		ro = filepath.Clean(strings.TrimSpace(ro))
		if !filepath.IsAbs(ro) {
			return Profile{}, fmt.Errorf("read_only %q: must be an absolute path", ro)
		}
		p.ReadOnly = append(p.ReadOnly, ro)
	}
	if yp.CopyConfig == nil {
		p.CopyConfig = append([]string(nil), defaultCopyConfig...)
	} else {
		for _, c := range *yp.CopyConfig {
			c = strings.TrimSpace(c)
			if c == "" || c != filepath.Base(c) || c == "." || c == ".." {
				return Profile{}, fmt.Errorf("copy_config %q: must be a plain entry name in the agent config dir", c)
			}
			if refusedConfigName(c) {
				return Profile{}, fmt.Errorf("copy_config %q: credential files are never copied into a sandbox", c)
			}
			p.CopyConfig = append(p.CopyConfig, c)
		}
	}
	return p, nil
}

func refusedConfigName(name string) bool {
	l := strings.ToLower(name)
	for _, r := range refusedConfigNames {
		if l == r {
			return true
		}
	}
	return strings.Contains(l, "credential")
}

// Resolve returns the named profile, or an error naming the ones that exist.
func Resolve(ryshDir, name string) (Profile, error) {
	name = strings.TrimSpace(name)
	if !ValidName(name) {
		return Profile{}, fmt.Errorf("sandbox profile %q: not a valid name", name)
	}
	all, err := LoadProfiles(ryshDir)
	if err != nil {
		return Profile{}, err
	}
	p, ok := all[name]
	if !ok {
		names := make([]string, 0, len(all))
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		return Profile{}, fmt.Errorf("sandbox profile %q not found (have: %s)", name, strings.Join(names, ", "))
	}
	return p, nil
}
