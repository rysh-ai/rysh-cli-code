// SPDX-License-Identifier: Apache-2.0

package main

// Tests for `rysh channel install|list|remove` (design 002, WS2 P3). All runs
// are rooted in a temp working directory so the project-local .rysh/channels
// registry stays sandboxed.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/plugin"
)

const testChannelManifest = `name = "mockx"
version = "0.2.0"
transport = "stdio"
exec = "run.sh"
declares_creds = ["MOCKX_TOKEN"]
declares_scopes = ["channel:mockx"]
network_egress = ["mockx.example.com:443"]
`

// writeTestPluginPackage creates a plugin package source directory.
func writeTestPluginPackage(t *testing.T, manifest string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "pkg-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "rysh.channel.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestChannelInstallListRemove(t *testing.T) {
	t.Chdir(t.TempDir())
	src := writeTestPluginPackage(t, testChannelManifest)

	// Install (explicit "y" confirmation): consent scan renders the declared
	// creds/scopes/egress before anything lands in .rysh/.
	var out bytes.Buffer
	if err := channelInstall(&out, strings.NewReader("y\n"), []string{src}); err != nil {
		t.Fatalf("channelInstall: %v", err)
	}
	for _, want := range []string{
		"MOCKX_TOKEN", "channel:mockx", "mockx.example.com:443",
		"signing tier: community", `installed channel plugin "mockx"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("install output missing %q:\n%s", want, out.String())
		}
	}
	installDir := filepath.Join(plugin.DefaultRoot, "mockx")
	for _, f := range []string{"rysh.channel.toml", "run.sh", "install.json"} {
		if _, err := os.Stat(filepath.Join(installDir, f)); err != nil {
			t.Fatalf("missing %s after install: %v", f, err)
		}
	}

	// List: built-ins + the installed plugin with transport/version/tier.
	out.Reset()
	if err := channelList(&out); err != nil {
		t.Fatalf("channelList: %v", err)
	}
	for _, want := range []string{
		"slack", "built-in",
		"mockx", "transport=stdio", "version=0.2.0", "tier=community",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list output missing %q:\n%s", want, out.String())
		}
	}

	// Remove with --yes.
	out.Reset()
	if err := channelRemove(&out, strings.NewReader(""), []string{"mockx", "--yes"}); err != nil {
		t.Fatalf("channelRemove: %v", err)
	}
	if _, err := os.Stat(installDir); !os.IsNotExist(err) {
		t.Fatal("plugin dir still present after remove")
	}
	if err := channelRemove(&out, strings.NewReader(""), []string{"mockx", "--yes"}); err == nil {
		t.Fatal("removing a non-installed plugin must error")
	}
}

func TestChannelInstallDeclined(t *testing.T) {
	t.Chdir(t.TempDir())
	src := writeTestPluginPackage(t, testChannelManifest)

	var out bytes.Buffer
	if err := channelInstall(&out, strings.NewReader("n\n"), []string{src}); err != nil {
		t.Fatalf("channelInstall (declined): %v", err)
	}
	if !strings.Contains(out.String(), "install aborted") {
		t.Fatalf("expected abort notice:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(plugin.DefaultRoot, "mockx")); !os.IsNotExist(err) {
		t.Fatal("declined install must not write anything")
	}
}

func TestChannelInstallYesFlagSkipsPrompt(t *testing.T) {
	t.Chdir(t.TempDir())
	src := writeTestPluginPackage(t, testChannelManifest)

	var out bytes.Buffer
	// Empty stdin: --yes must not read from it.
	if err := channelInstall(&out, strings.NewReader(""), []string{src, "--yes"}); err != nil {
		t.Fatalf("channelInstall --yes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plugin.DefaultRoot, "mockx")); err != nil {
		t.Fatalf("--yes install did not land: %v", err)
	}
}

func TestChannelInstallRejectsBuiltinName(t *testing.T) {
	t.Chdir(t.TempDir())
	src := writeTestPluginPackage(t, "name = \"slack\"\ntransport = \"stdio\"\nexec = \"run.sh\"\n")

	var out bytes.Buffer
	err := channelInstall(&out, strings.NewReader("y\n"), []string{src, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "collides with a built-in") {
		t.Fatalf("builtin-name install err = %v", err)
	}
}

func TestChannelInstallChecksumMismatch(t *testing.T) {
	t.Chdir(t.TempDir())
	// A directory source cannot be checksum-verified — and a bad checksum on
	// a tarball must reject. Directory + --checksum errors out cleanly:
	src := writeTestPluginPackage(t, testChannelManifest)
	var out bytes.Buffer
	err := channelInstall(&out, strings.NewReader(""), []string{src, "--yes", "--checksum", "sha256:" + strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "tarball") {
		t.Fatalf("dir+checksum err = %v", err)
	}
}

func TestChannelCmdUsage(t *testing.T) {
	var out bytes.Buffer
	if err := channelList(&out); err != nil {
		t.Fatalf("channelList: %v", err)
	}
	if err := channelInstall(&out, strings.NewReader(""), nil); err == nil {
		t.Fatal("install without args must error")
	}
	if err := channelRemove(&out, strings.NewReader(""), nil); err == nil {
		t.Fatal("remove without args must error")
	}
	if _, _, _, err := parseChannelFlags([]string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

// TestChannelSigningWorkflow drives the author/operator loop end to end at
// the CLI layer: keygen → sign → verify (untrusted) → trust → verify
// (dev-signed) → tamper → verify refuses.
func TestChannelSigningWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())
	src := writeTestPluginPackage(t, testChannelManifest)

	var out bytes.Buffer
	if err := channelKeygen(&out, nil); err != nil {
		t.Fatalf("channelKeygen: %v", err)
	}
	// The printed public key line is what an operator pastes into `trust`.
	var pubHex string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "public key: ") {
			pubHex = strings.TrimPrefix(line, "public key: ")
		}
	}
	if pubHex == "" {
		t.Fatalf("keygen output missing public key:\n%s", out.String())
	}

	out.Reset()
	if err := channelSign(&out, []string{src}); err != nil {
		t.Fatalf("channelSign: %v", err)
	}

	// Signed but not yet trusted: verify succeeds (loads) as community tier.
	out.Reset()
	if err := channelVerify(&out, []string{src}); err != nil {
		t.Fatalf("channelVerify(untrusted): %v", err)
	}
	if !strings.Contains(out.String(), "untrusted key") {
		t.Fatalf("verify output = %q, want untrusted-key tier", out.String())
	}

	out.Reset()
	if err := channelTrust(&out, []string{pubHex, "test", "key"}); err != nil {
		t.Fatalf("channelTrust: %v", err)
	}
	out.Reset()
	if err := channelVerify(&out, []string{src}); err != nil {
		t.Fatalf("channelVerify(trusted): %v", err)
	}
	if !strings.Contains(out.String(), "dev-signed by ") {
		t.Fatalf("verify output = %q, want dev-signed tier", out.String())
	}

	// Install carries the signature; list reports the tier.
	out.Reset()
	if err := channelInstall(&out, strings.NewReader(""), []string{src, "--yes"}); err != nil {
		t.Fatalf("channelInstall(signed): %v", err)
	}
	out.Reset()
	if err := channelList(&out); err != nil {
		t.Fatalf("channelList: %v", err)
	}
	if !strings.Contains(out.String(), "tier=dev-signed by ") {
		t.Fatalf("list output = %q, want dev-signed tier", out.String())
	}

	// Tamper the source package: verify now refuses (same verdict the loader
	// and installer apply).
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\ncurl evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := channelVerify(&out, []string{src}); err == nil ||
		!strings.Contains(err.Error(), "refuse") {
		t.Fatalf("channelVerify(tampered) err = %v, want refusal", err)
	}
}
