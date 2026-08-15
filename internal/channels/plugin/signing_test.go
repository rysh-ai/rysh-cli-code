// SPDX-License-Identifier: Apache-2.0

package plugin

// Dev-tier signing tests: sign → verify round-trip, tamper → refuse (the
// enforcement bite, at BOTH the loader hook and the supervisor spawn),
// unsigned / unknown-key → exactly the pre-signing load path.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// genTestKey writes a signing key under dir and returns the loaded private
// key plus a trust file trusting it.
func genTestKey(t *testing.T, dir string, trust bool) (ed25519.PrivateKey, string) {
	t.Helper()
	keyPath := filepath.Join(dir, "sign.key")
	pub, err := GenerateSigningKey(keyPath)
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	priv, err := LoadSigningKey(keyPath)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	trustPath := filepath.Join(dir, "plugin-keys")
	if trust {
		if err := AppendTrustedKey(trustPath, hex.EncodeToString(pub), "test key"); err != nil {
			t.Fatalf("AppendTrustedKey: %v", err)
		}
	}
	return priv, trustPath
}

// sigTestPluginDir lays out a signable plugin package with a relative exec.
func sigTestPluginDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	manifest := fmt.Sprintf("name = %q\nversion = \"1.0.0\"\ntransport = \"stdio\"\nexec = \"run.sh\"\n", name)
	writePluginDir(t, dir, manifest, map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"})
	return dir
}

func verifyDir(t *testing.T, dir, trustFile string) VerifyResult {
	t.Helper()
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest(%s): %v", dir, err)
	}
	return VerifyPlugin(dir, m, trustFile)
}

func TestSignVerifyRoundTrip(t *testing.T) {
	dir := sigTestPluginDir(t, "sigchan")
	priv, trustFile := genTestKey(t, t.TempDir(), true)

	// Unsigned first: the zero state.
	if res := verifyDir(t, dir, trustFile); res.Status != SigUnsigned {
		t.Fatalf("unsigned status = %v, want SigUnsigned", res.Status)
	}

	sf, err := SignPluginDir(dir, priv)
	if err != nil {
		t.Fatalf("SignPluginDir: %v", err)
	}
	if sf.KeyID == "" || sf.Algo != "ed25519" {
		t.Fatalf("signature file = %+v", sf)
	}

	res := verifyDir(t, dir, trustFile)
	if res.Status != SigTrusted {
		t.Fatalf("signed+trusted status = %v (%s), want SigTrusted", res.Status, res.Reason)
	}
	if res.KeyID != sf.KeyID {
		t.Fatalf("KeyID = %q, want %q", res.KeyID, sf.KeyID)
	}
	if !strings.Contains(res.TierString(), "dev-signed by "+sf.KeyID) {
		t.Fatalf("TierString = %q", res.TierString())
	}

	// Same signature, key NOT in the trust file → unknown key (loads anyway).
	if res := verifyDir(t, dir, filepath.Join(t.TempDir(), "no-keys")); res.Status != SigUnknownKey {
		t.Fatalf("untrusted status = %v, want SigUnknownKey", res.Status)
	}
}

func TestVerifyRefusesTamper(t *testing.T) {
	priv, trustFile := genTestKey(t, t.TempDir(), true)

	t.Run("binary", func(t *testing.T) {
		dir := sigTestPluginDir(t, "sigchan")
		if _, err := SignPluginDir(dir, priv); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\ncurl evil\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if res := verifyDir(t, dir, trustFile); res.Status != SigInvalid {
			t.Fatalf("tampered binary status = %v, want SigInvalid", res.Status)
		}
	})

	t.Run("manifest", func(t *testing.T) {
		dir := sigTestPluginDir(t, "sigchan")
		if _, err := SignPluginDir(dir, priv); err != nil {
			t.Fatal(err)
		}
		manifest := "name = \"sigchan\"\nversion = \"1.0.0\"\ntransport = \"stdio\"\nexec = \"run.sh\"\nnetwork_egress = [\"evil.example:443\"]\n"
		if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if res := verifyDir(t, dir, trustFile); res.Status != SigInvalid {
			t.Fatalf("tampered manifest status = %v, want SigInvalid", res.Status)
		}
	})

	t.Run("missing-binary", func(t *testing.T) {
		dir := sigTestPluginDir(t, "sigchan")
		if _, err := SignPluginDir(dir, priv); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "run.sh")); err != nil {
			t.Fatal(err)
		}
		if res := verifyDir(t, dir, trustFile); res.Status != SigInvalid {
			t.Fatalf("missing binary status = %v, want SigInvalid", res.Status)
		}
	})

	t.Run("garbage-sig-file", func(t *testing.T) {
		dir := sigTestPluginDir(t, "sigchan")
		if err := os.WriteFile(filepath.Join(dir, SignatureFileName), []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res := verifyDir(t, dir, trustFile); res.Status != SigInvalid {
			t.Fatalf("garbage sig status = %v, want SigInvalid", res.Status)
		}
	})

	t.Run("wrong-key-claim", func(t *testing.T) {
		// A sig file whose key_id does not match its own public key.
		dir := sigTestPluginDir(t, "sigchan")
		if _, err := SignPluginDir(dir, priv); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, SignatureFileName))
		if err != nil {
			t.Fatal(err)
		}
		var sf SignatureFile
		if err := json.Unmarshal(raw, &sf); err != nil {
			t.Fatal(err)
		}
		sf.KeyID = "0000000000000000"
		edited, _ := json.Marshal(sf)
		if err := os.WriteFile(filepath.Join(dir, SignatureFileName), edited, 0o644); err != nil {
			t.Fatal(err)
		}
		if res := verifyDir(t, dir, trustFile); res.Status != SigInvalid {
			t.Fatalf("key_id mismatch status = %v, want SigInvalid", res.Status)
		}
	})
}

func TestTrustFileParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin-keys")
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	content := "# comment\n\n" + hex.EncodeToString(pub) + " alice's dev key\nnot-a-key\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, bad, err := LoadTrustedKeys(path)
	if err != nil {
		t.Fatalf("LoadTrustedKeys: %v", err)
	}
	if len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("keys = %v", keys)
	}
	if len(bad) != 1 || bad[0] != "not-a-key" {
		t.Fatalf("bad = %v", bad)
	}

	// Missing file: empty trust set, no error.
	if keys, _, err := LoadTrustedKeys(filepath.Join(t.TempDir(), "nope")); err != nil || keys != nil {
		t.Fatalf("missing trust file: keys=%v err=%v", keys, err)
	}
}

func TestKeygenRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sign.key")
	if _, err := GenerateSigningKey(path); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateSigningKey(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite err = %v", err)
	}
}

// TestLoaderSignatureEnforcement drives the REAL load path
// (Wire → channels.NewAdapter): trusted loads and logs its tier, unknown-key
// and unsigned load exactly as before, tampered REFUSES. This test fails if
// the VerifyPlugin check in Wire's PluginLookup is reverted.
func TestLoaderSignatureEnforcement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mockchan")
	writePluginDir(t, dir, mockchanManifestTOML(t), nil)
	priv, trustFile := genTestKey(t, t.TempDir(), true)
	if _, err := SignPluginDir(dir, priv); err != nil {
		t.Fatalf("SignPluginDir: %v", err)
	}

	Wire(WireOptions{Root: root, TrustFile: trustFile, Supervisor: testOpts("MOCK_TRANSPORT=stdio")})
	defer Unwire()

	// Signed + trusted: loads and works end to end.
	adapter, err := channels.NewAdapter("mockchan", msg.ChannelConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewAdapter(signed+trusted): %v", err)
	}
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("Start(signed+trusted): %v", err)
	}
	recvInboundWith(t, adapter.InboundCh(), 5*time.Second, "inbound from signed plugin",
		func(im channels.InboundMessage) bool { return im.Content == "hello-from-plugin" })
	if err := adapter.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Tamper the signed manifest (grow its egress claim): must refuse to load.
	tampered := mockchanManifestTOML(t) + "network_egress = [\"evil.example:443\"]\n"
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := channels.NewAdapter("mockchan", msg.ChannelConfig{Enabled: true}); err == nil {
		t.Fatal("NewAdapter must refuse a signed-but-tampered plugin")
	}
	// The type is still "known" (it is installed) — only loading is refused.
	if !channels.IsValidChannelType("mockchan") {
		t.Fatal("tampered plugin should still be a known installed type")
	}

	// Unknown key: loads (pre-signing behavior). Re-sign the tampered
	// manifest with a key NOT in the trust file.
	otherPriv, _ := genTestKey(t, t.TempDir(), false)
	if _, err := SignPluginDir(dir, otherPriv); err != nil {
		t.Fatal(err)
	}
	if _, err := channels.NewAdapter("mockchan", msg.ChannelConfig{Enabled: true}); err != nil {
		t.Fatalf("NewAdapter(unknown key) must keep loading: %v", err)
	}

	// Unsigned: loads (pre-signing behavior).
	if err := os.Remove(filepath.Join(dir, SignatureFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := channels.NewAdapter("mockchan", msg.ChannelConfig{Enabled: true}); err != nil {
		t.Fatalf("NewAdapter(unsigned) must keep loading: %v", err)
	}
}

// TestSupervisorRefusesTamperedAtSpawn covers the second enforcement point:
// even with an adapter already constructed, spawn re-verifies and refuses.
func TestSupervisorRefusesTamperedAtSpawn(t *testing.T) {
	dir := sigTestPluginDir(t, "sigchan")
	priv, trustFile := genTestKey(t, t.TempDir(), true)
	if _, err := SignPluginDir(dir, priv); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err) // tamper AFTER signing
	}

	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOpts()
	opts.Dir = dir
	opts.TrustFile = trustFile
	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, opts)
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	err = a.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("Start on tampered dir = %v, want signature refusal", err)
	}
}

// TestInstallSignatureHandling: a broken signature never installs; a signed
// package records its verdict in the install record.
func TestInstallSignatureHandling(t *testing.T) {
	priv, _ := genTestKey(t, t.TempDir(), false)

	t.Run("tampered-refuses", func(t *testing.T) {
		src := sigTestPluginDir(t, "sigchan")
		if _, err := SignPluginDir(src, priv); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("tampered"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallPackage(t.TempDir(), src, ""); err == nil ||
			!strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("install of tampered package err = %v", err)
		}
	})

	t.Run("signed-records-tier", func(t *testing.T) {
		src := sigTestPluginDir(t, "sigchan")
		sf, err := SignPluginDir(src, priv)
		if err != nil {
			t.Fatal(err)
		}
		p, err := InstallPackage(t.TempDir(), src, "")
		if err != nil {
			t.Fatalf("InstallPackage: %v", err)
		}
		recData, err := os.ReadFile(filepath.Join(p.Dir, InstallRecordFileName))
		if err != nil {
			t.Fatal(err)
		}
		var rec InstallRecord
		if err := json.Unmarshal(recData, &rec); err != nil {
			t.Fatal(err)
		}
		// The signing key is not in the default trust file → untrusted tier,
		// but the key id is surfaced.
		if !strings.Contains(rec.Tier, sf.KeyID) {
			t.Fatalf("install record tier = %q, want key id %s in it", rec.Tier, sf.KeyID)
		}
	})
}
