// SPDX-License-Identifier: Apache-2.0

package plugin

// Dev-tier plugin signing (design 002 §4.6 "signing tiers", bottom tier made
// real). ed25519 detached signatures over a canonical digest of the manifest
// fields + the exec binary bytes, verified at plugin LOAD against a local
// trust file. No PKI, no registry service, no company signing identity: a key
// is trusted because the operator listed its public key in
// .rysh/plugin-keys. The manifest's own `signature` field stays reserved for
// the future registry flow (@rysh/@verified tiers) — the dev tier uses the
// detached rysh.channel.sig file so signing never rewrites the manifest.
//
// Enforcement semantics (the only three outcomes):
//   - no rysh.channel.sig            → SigUnsigned:   load, exactly as before
//   - sig verifies, key not trusted  → SigUnknownKey: load, exactly as before
//   - sig verifies, key trusted      → SigTrusted:    load, log "signed by <key-id>"
//   - sig present but broken/stale   → SigInvalid:    REFUSE to load
// A tampered binary or edited manifest under an existing signature is
// SigInvalid — deleting the sig file "only" demotes the plugin back to the
// unsigned tier, which is the honest limit of a local trust file.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// SignatureFileName is the detached signature shipped next to the manifest.
	SignatureFileName = "rysh.channel.sig"
	// DefaultTrustFile lists trusted ed25519 public keys, one hex key per
	// line, optional comment after whitespace, '#' lines ignored.
	// Project-local like the registry root (.rysh/channels).
	DefaultTrustFile = ".rysh/plugin-keys"
	// DefaultSigningKeyFile is where `rysh channel keygen` puts the private
	// key seed by default.
	DefaultSigningKeyFile = ".rysh/plugin-signing.key"

	sigAlgo        = "ed25519"
	seedKeyPrefix  = "rysh-ed25519-seed:" // private-key file format: prefix + hex(32-byte seed)
	digestPreamble = "rysh-channel-plugin-digest-v1"
)

// SignatureFile is the JSON content of rysh.channel.sig. The public key is
// embedded so verification is self-contained; TRUST is decided separately by
// the trust file, never by the sig file itself.
type SignatureFile struct {
	Algo      string `json:"algo"`       // "ed25519"
	KeyID     string `json:"key_id"`     // KeyID(public key)
	PublicKey string `json:"public_key"` // hex, 32 bytes
	Signature string `json:"signature"`  // hex, over PluginDigest
}

// SigStatus is the load-time signature verdict.
type SigStatus int

const (
	SigUnsigned   SigStatus = iota // no signature file — pre-signing behavior
	SigUnknownKey                  // valid signature, key not in the trust file — pre-signing behavior
	SigTrusted                     // valid signature by a trusted key
	SigInvalid                     // signature present but wrong — refuse to load
)

func (s SigStatus) String() string {
	switch s {
	case SigUnsigned:
		return "unsigned"
	case SigUnknownKey:
		return "signed (unknown key)"
	case SigTrusted:
		return "signed (trusted)"
	case SigInvalid:
		return "INVALID signature"
	}
	return "unknown"
}

// VerifyResult carries the verdict plus the key id (when a signature parsed)
// and the reason (when SigInvalid / SigUnknownKey).
type VerifyResult struct {
	Status SigStatus
	KeyID  string
	Reason string
}

// TierString renders the verdict for `rysh channel list` / install records.
func (r VerifyResult) TierString() string {
	switch r.Status {
	case SigTrusted:
		return "dev-signed by " + r.KeyID
	case SigUnknownKey:
		return "community (signed by untrusted key " + r.KeyID + ")"
	case SigInvalid:
		return "INVALID (" + r.Reason + ")"
	default:
		return "community (unsigned)"
	}
}

// KeyID is the first 16 hex chars of sha256 over the raw public key — short
// enough to log, long enough to not collide by accident.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// GenerateSigningKey creates an ed25519 keypair and writes the seed to path
// (0600). It refuses to overwrite an existing key file.
func GenerateSigningKey(path string) (ed25519.PublicKey, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("plugin keygen: %s already exists (delete it first if you really want a new key)", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("plugin keygen: %w", err)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("plugin keygen: %w", err)
		}
	}
	content := seedKeyPrefix + hex.EncodeToString(priv.Seed()) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("plugin keygen: %w", err)
	}
	return pub, nil
}

// LoadSigningKey reads a private key written by GenerateSigningKey.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plugin signing key: %w", err)
	}
	s := strings.TrimSpace(string(data))
	if !strings.HasPrefix(s, seedKeyPrefix) {
		return nil, fmt.Errorf("plugin signing key: %s is not a rysh signing key (want %q prefix)", path, seedKeyPrefix)
	}
	seed, err := hex.DecodeString(strings.TrimPrefix(s, seedKeyPrefix))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("plugin signing key: %s has a malformed seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PluginDigest computes the canonical bytes that get signed: a versioned
// preamble, the manifest fields, and the sha256 of the exec binary. The
// manifest's `checksum` and `signature` fields are EXCLUDED — both describe
// the package tarball for the registry flow and would be self-referential
// here. List fields join on \x1f so multi-value entries cannot collide with
// the line framing.
func PluginDigest(dir string, m Manifest) ([]byte, error) {
	binPath, err := execBinaryPath(dir, m)
	if err != nil {
		return nil, err
	}
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("plugin digest: read exec binary: %w", err)
	}
	binSum := sha256.Sum256(binData)

	join := func(items []string) string { return strings.Join(items, "\x1f") }
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", digestPreamble)
	fmt.Fprintf(&b, "name=%s\n", m.Name)
	fmt.Fprintf(&b, "version=%s\n", m.Version)
	fmt.Fprintf(&b, "transport=%s\n", m.Transport)
	fmt.Fprintf(&b, "exec=%s\n", m.Exec)
	fmt.Fprintf(&b, "min_rysh=%s\n", m.MinRysh)
	fmt.Fprintf(&b, "declares_creds=%s\n", join(m.DeclaresCreds))
	fmt.Fprintf(&b, "declares_scopes=%s\n", join(m.DeclaresScopes))
	fmt.Fprintf(&b, "network_egress=%s\n", join(m.NetworkEgress))
	fmt.Fprintf(&b, "binary_sha256=%s\n", hex.EncodeToString(binSum[:]))
	sum := sha256.Sum256([]byte(b.String()))
	return sum[:], nil
}

// execBinaryPath resolves the file whose bytes the signature covers: the
// first exec field, joined to dir when relative (the same resolution the
// supervisor uses at spawn).
func execBinaryPath(dir string, m Manifest) (string, error) {
	fields := strings.Fields(m.Exec)
	if len(fields) == 0 {
		return "", fmt.Errorf("plugin digest: empty exec in manifest")
	}
	bin := fields[0]
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(dir, bin)
	}
	return bin, nil
}

// SignPluginDir signs the plugin package at dir (manifest + exec binary) with
// the private key and writes rysh.channel.sig next to the manifest.
func SignPluginDir(dir string, priv ed25519.PrivateKey) (SignatureFile, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return SignatureFile{}, err
	}
	digest, err := PluginDigest(dir, m)
	if err != nil {
		return SignatureFile{}, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	sf := SignatureFile{
		Algo:      sigAlgo,
		KeyID:     KeyID(pub),
		PublicKey: hex.EncodeToString(pub),
		Signature: hex.EncodeToString(ed25519.Sign(priv, digest)),
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return SignatureFile{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, SignatureFileName), append(data, '\n'), 0o644); err != nil {
		return SignatureFile{}, fmt.Errorf("plugin sign: %w", err)
	}
	return sf, nil
}

// LoadTrustedKeys parses the trust file: one hex ed25519 public key per line,
// optional comment after whitespace; '#' lines and blanks ignored. A missing
// file is an empty trust set, not an error. Malformed lines are skipped
// (returned in bad) so one typo cannot lock every plugin out.
func LoadTrustedKeys(path string) (keys []ed25519.PublicKey, bad []string, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("plugin trust file: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hexKey := strings.Fields(line)[0]
		raw, derr := hex.DecodeString(hexKey)
		if derr != nil || len(raw) != ed25519.PublicKeySize {
			bad = append(bad, hexKey)
			continue
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, bad, nil
}

// AppendTrustedKey adds a hex public key (+ optional comment) to the trust
// file, creating it if needed.
func AppendTrustedKey(path, hexKey, comment string) error {
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("plugin trust: %q is not a %d-byte hex ed25519 public key", hexKey, ed25519.PublicKeySize)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("plugin trust: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("plugin trust: %w", err)
	}
	defer f.Close()
	line := hexKey
	if comment != "" {
		line += " " + comment
	}
	_, err = f.WriteString(line + "\n")
	return err
}

// VerifyPlugin evaluates the plugin at dir against the trust file. It never
// returns a Go error: every failure mode maps onto a SigStatus so callers get
// exactly one enforcement decision (see the package-top semantics).
func VerifyPlugin(dir string, m Manifest, trustFile string) VerifyResult {
	if trustFile == "" {
		trustFile = DefaultTrustFile
	}
	sigPath := filepath.Join(dir, SignatureFileName)
	data, err := os.ReadFile(sigPath)
	if os.IsNotExist(err) {
		return VerifyResult{Status: SigUnsigned}
	}
	if err != nil {
		return VerifyResult{Status: SigInvalid, Reason: fmt.Sprintf("unreadable %s: %v", SignatureFileName, err)}
	}

	var sf SignatureFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return VerifyResult{Status: SigInvalid, Reason: fmt.Sprintf("malformed %s: %v", SignatureFileName, err)}
	}
	if sf.Algo != sigAlgo {
		return VerifyResult{Status: SigInvalid, KeyID: sf.KeyID, Reason: fmt.Sprintf("unsupported algo %q", sf.Algo)}
	}
	pubRaw, err := hex.DecodeString(sf.PublicKey)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		return VerifyResult{Status: SigInvalid, KeyID: sf.KeyID, Reason: "malformed public key"}
	}
	pub := ed25519.PublicKey(pubRaw)
	if got := KeyID(pub); got != sf.KeyID {
		return VerifyResult{Status: SigInvalid, KeyID: sf.KeyID, Reason: "key_id does not match public key"}
	}
	sig, err := hex.DecodeString(sf.Signature)
	if err != nil {
		return VerifyResult{Status: SigInvalid, KeyID: sf.KeyID, Reason: "malformed signature"}
	}

	digest, err := PluginDigest(dir, m)
	if err != nil {
		// A signed plugin whose exec binary vanished is tampered-by-absence.
		return VerifyResult{Status: SigInvalid, KeyID: sf.KeyID, Reason: err.Error()}
	}
	if !ed25519.Verify(pub, digest, sig) {
		return VerifyResult{Status: SigInvalid, KeyID: sf.KeyID,
			Reason: "signature does not match manifest + binary (tampered or re-packed without re-signing)"}
	}

	trusted, _, terr := LoadTrustedKeys(trustFile)
	if terr != nil {
		// Trust file unreadable ⇒ nothing is trusted; the signature itself
		// verified, so this is the unknown-key (load-anyway) path, not refusal.
		return VerifyResult{Status: SigUnknownKey, KeyID: sf.KeyID,
			Reason: fmt.Sprintf("trust file: %v", terr)}
	}
	for _, k := range trusted {
		if k.Equal(pub) {
			return VerifyResult{Status: SigTrusted, KeyID: sf.KeyID}
		}
	}
	return VerifyResult{Status: SigUnknownKey, KeyID: sf.KeyID,
		Reason: fmt.Sprintf("key %s not in %s", sf.KeyID, trustFile)}
}
