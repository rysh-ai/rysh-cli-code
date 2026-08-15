// SPDX-License-Identifier: Apache-2.0

package plugin

// Registry — installed channel plugins on disk, plus the wiring that lets
// channels.NewAdapter resolve them without the channels package importing
// this one (nil-safe hook func vars set by Wire; see channels/factory.go).
//
// Registry root: PROJECT-LOCAL `.rysh/channels/{name}/` (manifest + binary +
// install record). This is a DELIBERATE deviation from design 002 §4.4, which
// sketched `~/.rysh/channels/`: design 004 §4.2 commits rysh to project-local
// `.rysh/` state with no global root (the same convention as
// .rysh/channel-state/), so channel plugins are installed per project.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// DefaultRoot is the project-local registry directory.
const DefaultRoot = ".rysh/channels"

// InstalledPlugin is one installed plugin: its manifest and install dir.
type InstalledPlugin struct {
	Manifest Manifest
	Dir      string
}

// Registry scans installed plugin manifests under a root directory. Lookups
// re-scan on demand: installs/removals in the same process (or by another
// process) are visible immediately, and the scan is a couple of stats.
type Registry struct {
	root string
}

// LoadRegistry opens the registry rooted at root (DefaultRoot when empty). A
// missing root is not an error — the registry is just empty.
func LoadRegistry(root string) (*Registry, error) {
	if root == "" {
		root = DefaultRoot
	}
	if st, err := os.Stat(root); err == nil && !st.IsDir() {
		return nil, fmt.Errorf("plugin registry: %s is not a directory", root)
	}
	return &Registry{root: root}, nil
}

// Root returns the registry root directory.
func (r *Registry) Root() string { return r.root }

// Lookup resolves an installed plugin by channel-type name. Names colliding
// with built-ins are never resolvable (second enforcement point after
// install-time validation), and the manifest must agree with its directory
// name.
func (r *Registry) Lookup(name string) (InstalledPlugin, bool) {
	if !pluginNameRe.MatchString(name) || isBuiltinChannelType(name) {
		return InstalledPlugin{}, false
	}
	dir := filepath.Join(r.root, name)
	m, err := LoadManifest(dir)
	if err != nil {
		return InstalledPlugin{}, false
	}
	if m.Name != name {
		slog.Warn("plugin registry: manifest name disagrees with install dir, ignoring",
			"dir", dir, "manifest_name", m.Name)
		return InstalledPlugin{}, false
	}
	return InstalledPlugin{Manifest: m, Dir: dir}, true
}

// List returns all installed plugins, sorted by name. Broken or
// builtin-colliding entries are skipped with a warning.
func (r *Registry) List() []InstalledPlugin {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil
	}
	var out []InstalledPlugin
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p, ok := r.Lookup(e.Name())
		if !ok {
			slog.Warn("plugin registry: skipping invalid entry", "dir", filepath.Join(r.root, e.Name()))
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out
}

// NATSProvider lazily yields the core-side bus handle plus the client URL a
// nats-transport plugin process is handed via env. Nil when the process has
// no session bus (stdio plugins are unaffected).
type NATSProvider func() (*nats.Conn, string, error)

// WireOptions configures the channels-package hooks.
type WireOptions struct {
	// Root of the plugin registry (DefaultRoot when empty).
	Root string
	// TrustFile lists trusted signing keys (DefaultTrustFile when empty).
	TrustFile string
	// NATS provides the session bus for nats-transport plugins; nil makes
	// nats plugins unresolvable (logged), stdio plugins still work.
	NATS NATSProvider
	// Supervisor is the base supervisor option set (backoffs etc.); Dir,
	// NATSConn, and NATSURL are filled per plugin at resolution time.
	Supervisor SupervisorOptions
}

// Wire installs the nil-safe hooks in the channels package so
// channels.NewAdapter (built-ins first, always) falls through to the plugin
// registry, and channels.IsValidChannelType accepts installed plugin names.
// The channels package deliberately does NOT import this package — the CLI
// layer calls Wire once at startup.
func Wire(o WireOptions) {
	channels.PluginTypeKnown = func(channelType string) bool {
		reg, err := LoadRegistry(o.Root)
		if err != nil {
			return false
		}
		_, ok := reg.Lookup(channelType)
		return ok
	}

	channels.PluginLookup = func(channelType string, config msg.ChannelConfig) (channels.ChannelAdapter, bool) {
		reg, err := LoadRegistry(o.Root)
		if err != nil {
			slog.Warn("plugin: registry unavailable", "err", err)
			return nil, false
		}
		p, ok := reg.Lookup(channelType)
		if !ok {
			return nil, false
		}

		// Load-time signature check (dev signing tier). Only a present-but-
		// wrong signature refuses; unsigned and unknown-key keep the
		// pre-signing behavior. The supervisor re-checks before every spawn,
		// so a binary swapped after this point is caught on (re)start too.
		switch res := VerifyPlugin(p.Dir, p.Manifest, o.TrustFile); res.Status {
		case SigInvalid:
			slog.Error("plugin: signature verification FAILED, refusing to load",
				"plugin", channelType, "dir", p.Dir, "key_id", res.KeyID, "reason", res.Reason)
			return nil, false
		case SigTrusted:
			slog.Info("plugin: signed by trusted key", "plugin", channelType, "key_id", res.KeyID)
		case SigUnknownKey:
			slog.Debug("plugin: signature by untrusted key, treating as unsigned",
				"plugin", channelType, "key_id", res.KeyID, "reason", res.Reason)
		}

		opts := o.Supervisor
		opts.Dir = p.Dir
		opts.TrustFile = o.TrustFile
		if p.Manifest.Transport == TransportNATS {
			if o.NATS == nil {
				slog.Warn("plugin: nats-transport plugin needs a session bus, none wired",
					"plugin", channelType)
				return nil, false
			}
			nc, url, err := o.NATS()
			if err != nil {
				slog.Warn("plugin: session bus unavailable for nats-transport plugin",
					"plugin", channelType, "err", err)
				return nil, false
			}
			opts.NATSConn, opts.NATSURL = nc, url
		}

		adapter, err := NewPluginChannelAdapter(p.Manifest, config, opts)
		if err != nil {
			slog.Warn("plugin: adapter construction failed", "plugin", channelType, "err", err)
			return nil, false
		}
		slog.Debug("plugin: resolved installed channel plugin",
			"plugin", channelType, "transport", p.Manifest.Transport, "dir", p.Dir)
		return adapter, true
	}
}

// Unwire clears the hooks (test hygiene).
func Unwire() {
	channels.PluginTypeKnown = nil
	channels.PluginLookup = nil
}
