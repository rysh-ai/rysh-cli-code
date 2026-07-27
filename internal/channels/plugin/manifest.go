// Package plugin implements the out-of-process channel plugin SDK
// (openclaw_roadmap design 002, WS2 P1-P3): a third party ships a channel as a
// separate process speaking a small wire contract, and the in-core
// PluginChannelAdapter shim proxies the ChannelAdapter interface to it over
// stdio JSON-RPC (fallback) or the embedded per-session NATS bus (preferred).
// The channels package never imports this package — it reaches installed
// plugins through the nil-safe channels.PluginLookup / channels.PluginTypeKnown
// hooks that Wire installs.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// ManifestFileName is the manifest file every channel plugin package ships at
// its root (design 002 §4.3).
const ManifestFileName = "rysh.channel.toml"

// Transport values a manifest may declare.
const (
	TransportNATS  = "nats"
	TransportStdio = "stdio"
)

// pluginNameRe constrains plugin channel-type names: lowercase, digits,
// hyphens, 2-32 chars, starting with a letter.
var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Manifest is the parsed rysh.channel.toml (design 002 §4.3).
type Manifest struct {
	Name      string `toml:"name"`      // channel type string; must not collide with a built-in
	Version   string `toml:"version"`   // plugin version, informational
	Transport string `toml:"transport"` // "nats" (preferred) | "stdio"
	Exec      string `toml:"exec"`      // command the supervisor launches (path within the package; whitespace-split into argv)
	MinRysh   string `toml:"min_rysh"`  // minimum rysh version, informational in v1
	Checksum  string `toml:"checksum"`  // "sha256:<hex>" over the package tarball (registry flow)
	Signature string `toml:"signature"` // reserved for the registry flow (@rysh/@verified); the dev tier signs via the detached rysh.channel.sig instead (signing.go)

	DeclaresCreds  []string `toml:"declares_creds"`  // ${ENV} credentials the plugin needs
	DeclaresScopes []string `toml:"declares_scopes"` // capabilities requested
	NetworkEgress  []string `toml:"network_egress"`  // hosts the plugin declares it reaches (declared, not enforced in v1)
}

// ParseManifest decodes and validates a rysh.channel.toml document.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest: parse %s: %w", ManifestFileName, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// LoadManifest reads and validates dir/rysh.channel.toml.
func LoadManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest: %w", err)
	}
	return ParseManifest(data)
}

// Validate checks the manifest invariants: a well-formed name that does not
// collide with a built-in channel type, a known transport, and a non-empty
// exec. Built-in collision is enforced here (install time) AND again at
// factory resolution order (built-in switch cases run first), so a plugin
// named "slack" can never shadow the built-in Slack adapter.
func (m Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("plugin manifest: name is required")
	}
	if !pluginNameRe.MatchString(m.Name) {
		return fmt.Errorf("plugin manifest: invalid name %q (must match %s)", m.Name, pluginNameRe.String())
	}
	if isBuiltinChannelType(m.Name) {
		return fmt.Errorf("plugin manifest: name %q collides with a built-in channel type", m.Name)
	}
	switch m.Transport {
	case TransportNATS, TransportStdio:
	case "":
		return fmt.Errorf("plugin manifest: transport is required (%q or %q)", TransportNATS, TransportStdio)
	default:
		return fmt.Errorf("plugin manifest: unsupported transport %q (want %q or %q)", m.Transport, TransportNATS, TransportStdio)
	}
	if strings.TrimSpace(m.Exec) == "" {
		return fmt.Errorf("plugin manifest: exec is required")
	}
	return nil
}

// isBuiltinChannelType checks against the built-in slice only (NOT
// channels.IsValidChannelType, which also accepts installed plugins via the
// PluginTypeKnown hook — using it here would make every installed plugin
// "collide" with itself).
func isBuiltinChannelType(t string) bool {
	for _, b := range channels.ValidChannelTypes {
		if t == b {
			return true
		}
	}
	return false
}

// SigningTier reports the tier knowable from the MANIFEST ALONE: always
// "community". The registry tiers (@rysh/@verified, design 005) still need
// the registry service's key infrastructure and the manifest `signature`
// field stays a documented stub for them. The dev tier is real but lives
// outside the manifest — a detached rysh.channel.sig verified against the
// local trust file (signing.go); use VerifyPlugin(...).TierString() when a
// package directory is at hand.
func (m Manifest) SigningTier() string {
	return "community"
}

// signatureStubNotice is printed verbatim when a manifest carries a
// `signature` value (still a registry-flow stub — see SigningTier).
const signatureStubNotice = "manifest signature field present (registry-tier verification not implemented — dev tier uses rysh.channel.sig)"

// ConsentText renders the install-time consent scan (design 002 §4.4): the
// declared credentials, scopes, and network egress, plus the signing tier.
// Pure function — `rysh channel install` prints it before asking for
// confirmation, and tests assert on it.
func ConsentText(m Manifest) string {
	var b strings.Builder
	version := m.Version
	if version == "" {
		version = "(unversioned)"
	}
	fmt.Fprintf(&b, "Channel plugin %q %s — transport: %s\n", m.Name, version, m.Transport)
	fmt.Fprintf(&b, "  signing tier: %s (checksum only)\n", m.SigningTier())
	if m.Signature != "" {
		fmt.Fprintf(&b, "  %s\n", signatureStubNotice)
	}

	writeList := func(label string, items []string, empty string) {
		if len(items) == 0 {
			fmt.Fprintf(&b, "  %s: %s\n", label, empty)
			return
		}
		fmt.Fprintf(&b, "  %s:\n", label)
		for _, it := range items {
			fmt.Fprintf(&b, "    - %s\n", it)
		}
	}
	writeList("declared credentials (${ENV} the plugin reads)", m.DeclaresCreds, "none declared")
	writeList("declared scopes", m.DeclaresScopes, "none declared")
	writeList("declared network egress (NOT enforced by rysh in v1)", m.NetworkEgress, "none declared")

	b.WriteString("  nothing runs at install time; the plugin process starts only when a humanoid starts this channel\n")
	return b.String()
}

// VerifyChecksum computes sha256 over the file at path and compares it with
// expected ("sha256:<hex>" or bare hex).
func VerifyChecksum(path, expected string) error {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(expected), "sha256:"))
	if want == "" {
		return fmt.Errorf("plugin checksum: empty expected checksum")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("plugin checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("plugin checksum: read %s: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("plugin checksum mismatch for %s: got sha256:%s, want sha256:%s", filepath.Base(path), got, want)
	}
	return nil
}
