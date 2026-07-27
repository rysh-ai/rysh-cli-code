package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodManifestTOML = `
name        = "matrix"
version     = "0.3.1"
transport   = "nats"
exec        = "rysh-channel-matrix"
min_rysh    = "0.9.0"
checksum    = "sha256:abc123"
signature   = "sig-bytes"

declares_creds   = ["MATRIX_TOKEN", "MATRIX_HS_URL"]
declares_scopes  = ["channel:matrix"]
network_egress   = ["matrix.org:443"]
`

func TestParseManifestGood(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifestTOML))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "matrix" || m.Version != "0.3.1" || m.Transport != "nats" ||
		m.Exec != "rysh-channel-matrix" || m.MinRysh != "0.9.0" {
		t.Fatalf("fields wrong: %+v", m)
	}
	if len(m.DeclaresCreds) != 2 || m.DeclaresCreds[0] != "MATRIX_TOKEN" {
		t.Fatalf("creds = %v", m.DeclaresCreds)
	}
	if len(m.DeclaresScopes) != 1 || len(m.NetworkEgress) != 1 {
		t.Fatalf("scopes/egress = %v / %v", m.DeclaresScopes, m.NetworkEgress)
	}
}

func TestManifestValidateTable(t *testing.T) {
	base := func() Manifest {
		return Manifest{Name: "matrix", Transport: "stdio", Exec: "run"}
	}
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"good", func(*Manifest) {}, ""},
		{"good nats", func(m *Manifest) { m.Transport = "nats" }, ""},
		{"missing name", func(m *Manifest) { m.Name = "" }, "name is required"},
		{"uppercase name", func(m *Manifest) { m.Name = "Matrix" }, "invalid name"},
		{"single-char name", func(m *Manifest) { m.Name = "x" }, "invalid name"},
		{"digit-leading name", func(m *Manifest) { m.Name = "9chan" }, "invalid name"},
		{"underscore name", func(m *Manifest) { m.Name = "my_chan" }, "invalid name"},
		{"too-long name", func(m *Manifest) { m.Name = "a" + strings.Repeat("b", 32) }, "invalid name"},
		{"builtin collision slack", func(m *Manifest) { m.Name = "slack" }, "collides with a built-in"},
		{"builtin collision imessage", func(m *Manifest) { m.Name = "imessage" }, "collides with a built-in"},
		{"missing transport", func(m *Manifest) { m.Transport = "" }, "transport is required"},
		{"bad transport", func(m *Manifest) { m.Transport = "grpc" }, "unsupported transport"},
		{"missing exec", func(m *Manifest) { m.Exec = "  " }, "exec is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(&m)
			err := m.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseManifestBadTOML(t *testing.T) {
	if _, err := ParseManifest([]byte("name = [broken")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestConsentText(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifestTOML))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	text := ConsentText(m)

	for _, want := range []string{
		`Channel plugin "matrix" 0.3.1`,
		"transport: nats",
		"signing tier: community",
		signatureStubNotice, // honest v1 stub: never fake crypto
		"MATRIX_TOKEN",
		"MATRIX_HS_URL",
		"channel:matrix",
		"matrix.org:443",
		"NOT enforced by rysh in v1",
		"nothing runs at install time",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("consent text missing %q:\n%s", want, text)
		}
	}

	// Unsigned + nothing declared: the empty markers render, the stub notice
	// does not.
	bare := Manifest{Name: "bare", Transport: "stdio", Exec: "run"}
	text = ConsentText(bare)
	if strings.Contains(text, signatureStubNotice) {
		t.Fatalf("unsigned plugin must not print the signature notice:\n%s", text)
	}
	if strings.Count(text, "none declared") != 3 {
		t.Fatalf("expected 3 'none declared' markers:\n%s", text)
	}
}

func TestVerifyChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pkg.tar.gz")
	content := []byte("not really a tarball, but bytes are bytes")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	good := "sha256:" + hex.EncodeToString(sum[:])

	if err := VerifyChecksum(path, good); err != nil {
		t.Fatalf("matching checksum rejected: %v", err)
	}
	if err := VerifyChecksum(path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("bare-hex checksum rejected: %v", err)
	}
	if err := VerifyChecksum(path, "sha256:"+strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatch not rejected: %v", err)
	}
	if err := VerifyChecksum(path, ""); err == nil {
		t.Fatal("empty expected checksum must error")
	}
}
