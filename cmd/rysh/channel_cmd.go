// SPDX-License-Identifier: Apache-2.0

package main

// channel_cmd.go — `rysh channel install|list|remove` for out-of-process
// channel plugins (openclaw_roadmap design 002, WS2 P3).
//
// v1 installs from a LOCAL package (a .tar.gz/.tgz tarball or a directory);
// fetching from a registry service (design 005) is a documented TODO. Nothing
// executes at install time: the package is consent-scanned, verified, and
// copied into the project-local .rysh/channels/{name}/ — the plugin process
// starts only when a humanoid starts the channel.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/channels/plugin"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// init wires the channel-plugin registry hooks into the channels package so
// channels.NewAdapter / channels.IsValidChannelType (called from the daemon's
// HumanoidActor and skill-file validation) resolve installed plugins. This is
// the CLI-layer Wire call design 002 prescribes — the channels package itself
// never imports the plugin package.
func init() {
	plugin.Wire(plugin.WireOptions{
		Root: plugin.DefaultRoot,
		NATS: defaultPluginNATSProvider,
	})
}

// defaultPluginNATSProvider lazily yields the session bus handle a
// nats-transport plugin needs: RYSH_PLUGIN_NATS_URL when set, otherwise the
// current session's recorded loopback NATS port. Only invoked when a
// nats-transport plugin channel is actually started; stdio plugins never
// touch it.
func defaultPluginNATSProvider() (*nats.Conn, string, error) {
	url := os.Getenv("RYSH_PLUGIN_NATS_URL")
	if url == "" {
		cfg := config.Load()
		store, err := session.NewStore(cfg)
		if err != nil {
			return nil, "", fmt.Errorf("plugin nats: session store: %w", err)
		}
		rec, err := store.Get(cfg.SessionName)
		if err != nil || rec.NATSPort <= 0 {
			return nil, "", fmt.Errorf("plugin nats: no running session bus found (session %q)", cfg.SessionName)
		}
		url = fmt.Sprintf("nats://127.0.0.1:%d", rec.NATSPort)
	}
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, "", fmt.Errorf("plugin nats: connect %s: %w", url, err)
	}
	return nc, url, nil
}

// runChannelCmd dispatches the `rysh channel` subcommands. The signing
// subcommands (keygen/sign/verify/trust) live here rather than a separate
// cmd/plugin-sign binary: they are a few dozen lines each and authors already
// have rysh installed — a second binary would be heavier for no isolation win.
func runChannelCmd(cfg config.Config, args []string) error {
	_ = cfg // registry root is project-local (.rysh/channels), like .rysh/channel-state
	if len(args) == 0 {
		return errors.New(progname.Rewrite("usage: rysh channel <install|list|remove|keygen|sign|verify|trust> [args]"))
	}
	switch args[0] {
	case "install":
		return channelInstall(os.Stdout, os.Stdin, args[1:])
	case "list":
		return channelList(os.Stdout)
	case "remove":
		return channelRemove(os.Stdout, os.Stdin, args[1:])
	case "keygen":
		return channelKeygen(os.Stdout, args[1:])
	case "sign":
		return channelSign(os.Stdout, args[1:])
	case "verify":
		return channelVerify(os.Stdout, args[1:])
	case "trust":
		return channelTrust(os.Stdout, args[1:])
	default:
		return fmt.Errorf(progname.Rewrite("rysh channel: unknown subcommand %q (want install|list|remove|keygen|sign|verify|trust)"), args[0])
	}
}

// parseChannelFlags splits positional args from --yes / --checksum flags.
func parseChannelFlags(args []string) (pos []string, yes bool, checksum string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--checksum":
			if i+1 >= len(args) {
				return nil, false, "", fmt.Errorf("--checksum requires a value (sha256:<hex>)")
			}
			i++
			checksum = args[i]
		case strings.HasPrefix(a, "--checksum="):
			checksum = strings.TrimPrefix(a, "--checksum=")
		case strings.HasPrefix(a, "-"):
			return nil, false, "", fmt.Errorf("unknown flag %q", a)
		default:
			pos = append(pos, a)
		}
	}
	return pos, yes, checksum, nil
}

// confirm prints prompt and requires an explicit "y"/"yes" line on in.
func confirm(out io.Writer, in io.Reader, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// channelInstall implements `rysh channel install <pkg.tar.gz|dir> [--yes]
// [--checksum sha256:<hex>]`.
func channelInstall(out io.Writer, in io.Reader, args []string) error {
	pos, yes, checksum, err := parseChannelFlags(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(progname.Rewrite("usage: rysh channel install <path-to-package.tar.gz|dir> [--yes] [--checksum sha256:<hex>]"))
	}
	src := pos[0]

	// Read + validate the manifest without installing anything, then render
	// the consent scan (declared creds/scopes/egress + signing tier).
	m, err := plugin.ReadPackageManifest(src)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprint(out, plugin.ConsentText(m))
	fmt.Fprintln(out)

	if !yes && !confirm(out, in, fmt.Sprintf("Install channel plugin %q? [y/N] ", m.Name)) {
		fmt.Fprintln(out, "install aborted")
		return nil
	}

	installed, err := plugin.InstallPackage(plugin.DefaultRoot, src, checksum)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed channel plugin %q %s (transport %s) into %s\n",
		installed.Manifest.Name, installed.Manifest.Version, installed.Manifest.Transport, installed.Dir)
	fmt.Fprintf(out, "use it in a humanoid skill file as a `contacts: %s:` channel\n", installed.Manifest.Name)
	return nil
}

// channelList implements `rysh channel list`: built-ins + installed plugins.
func channelList(out io.Writer) error {
	fmt.Fprintln(out, "built-in channels:")
	for _, t := range channels.ValidChannelTypes {
		fmt.Fprintf(out, "  %-12s built-in\n", t)
	}

	reg, err := plugin.LoadRegistry(plugin.DefaultRoot)
	if err != nil {
		return err
	}
	plugins := reg.List()
	fmt.Fprintln(out, "installed channel plugins:")
	if len(plugins) == 0 {
		fmt.Fprintln(out, progname.Rewrite("  (none — `rysh channel install <pkg>`)"))
		return nil
	}
	for _, p := range plugins {
		version := p.Manifest.Version
		if version == "" {
			version = "-"
		}
		verdict := plugin.VerifyPlugin(p.Dir, p.Manifest, plugin.DefaultTrustFile)
		fmt.Fprintf(out, "  %-12s plugin  transport=%s  version=%s  tier=%s\n",
			p.Manifest.Name, p.Manifest.Transport, version, verdict.TierString())
		if verdict.Status == plugin.SigInvalid {
			fmt.Fprintf(out, "  %-12s         ^ will REFUSE to load: %s\n", "", verdict.Reason)
		}
	}
	return nil
}

// channelKeygen implements `rysh channel keygen [--out <file>]`: a dev-tier
// ed25519 signing keypair (no company identity, no PKI — the trust decision
// is the local .rysh/plugin-keys file).
func channelKeygen(out io.Writer, args []string) error {
	path := plugin.DefaultSigningKeyFile
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--out":
			if i+1 >= len(args) {
				return fmt.Errorf("--out requires a path")
			}
			i++
			path = args[i]
		case strings.HasPrefix(args[i], "--out="):
			path = strings.TrimPrefix(args[i], "--out=")
		default:
			return errors.New(progname.Rewrite("usage: rysh channel keygen [--out <file>]"))
		}
	}
	pub, err := plugin.GenerateSigningKey(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote private signing key to %s (keep it out of git)\n", path)
	fmt.Fprintf(out, "public key: %x\n", []byte(pub))
	fmt.Fprintf(out, "key id:     %s\n", plugin.KeyID(pub))
	fmt.Fprintf(out, progname.Rewrite("to trust plugins signed with it: rysh channel trust %x [comment]\n"), []byte(pub))
	return nil
}

// channelSign implements `rysh channel sign <plugin-dir> [--key <file>]`:
// writes a detached rysh.channel.sig over the manifest fields + exec binary.
func channelSign(out io.Writer, args []string) error {
	keyPath := plugin.DefaultSigningKeyFile
	var pos []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--key":
			if i+1 >= len(args) {
				return fmt.Errorf("--key requires a path")
			}
			i++
			keyPath = args[i]
		case strings.HasPrefix(args[i], "--key="):
			keyPath = strings.TrimPrefix(args[i], "--key=")
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 1 {
		return fmt.Errorf(progname.Rewrite("usage: rysh channel sign <plugin-dir> [--key <file>] (default key: %s — `rysh channel keygen` creates one)"), plugin.DefaultSigningKeyFile)
	}
	priv, err := plugin.LoadSigningKey(keyPath)
	if err != nil {
		return err
	}
	sf, err := plugin.SignPluginDir(pos[0], priv)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed %s with key %s → %s/%s\n", pos[0], sf.KeyID, pos[0], plugin.SignatureFileName)
	fmt.Fprintf(out, "re-run after ANY change to the manifest or the exec binary — a stale signature refuses to load\n")
	return nil
}

// channelVerify implements `rysh channel verify <plugin-dir>`: prints the
// load-time verdict; non-zero exit iff the plugin would refuse to load.
func channelVerify(out io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New(progname.Rewrite("usage: rysh channel verify <plugin-dir>"))
	}
	m, err := plugin.LoadManifest(args[0])
	if err != nil {
		return err
	}
	verdict := plugin.VerifyPlugin(args[0], m, plugin.DefaultTrustFile)
	fmt.Fprintf(out, "%s: %s\n", m.Name, verdict.TierString())
	if verdict.Status == plugin.SigInvalid {
		return fmt.Errorf("plugin %q would refuse to load: %s", m.Name, verdict.Reason)
	}
	return nil
}

// channelTrust implements `rysh channel trust <hex-pubkey> [comment...]`:
// appends the key to the local trust file.
func channelTrust(out io.Writer, args []string) error {
	if len(args) < 1 {
		return errors.New(progname.Rewrite("usage: rysh channel trust <hex-ed25519-pubkey> [comment...]"))
	}
	if err := plugin.AppendTrustedKey(plugin.DefaultTrustFile, args[0], strings.Join(args[1:], " ")); err != nil {
		return err
	}
	fmt.Fprintf(out, "added key to %s — plugins signed with it now load as \"dev-signed\"\n", plugin.DefaultTrustFile)
	return nil
}

// channelRemove implements `rysh channel remove <name> [--yes]`.
func channelRemove(out io.Writer, in io.Reader, args []string) error {
	pos, yes, _, err := parseChannelFlags(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(progname.Rewrite("usage: rysh channel remove <name> [--yes]"))
	}
	name := pos[0]

	reg, err := plugin.LoadRegistry(plugin.DefaultRoot)
	if err != nil {
		return err
	}
	if _, ok := reg.Lookup(name); !ok {
		return fmt.Errorf("channel plugin %q is not installed", name)
	}
	if !yes && !confirm(out, in, fmt.Sprintf("Remove channel plugin %q? [y/N] ", name)) {
		fmt.Fprintln(out, "remove aborted")
		return nil
	}
	if err := plugin.Remove(plugin.DefaultRoot, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed channel plugin %q\n", name)
	return nil
}
