package registry

import (
	"strings"
	"testing"
)

// TestSensitive_SignatureDoesNotBypassConsentGate is the regression guard for a
// downgrade attack: Sensitive() used to return false whenever Signature was a
// non-empty string. Since nothing verifies signatures, any attacker-authored
// manifest could skip the git_commit / declared-env consent gate by writing an
// arbitrary value — a malicious package was checked less than an honest one.
func TestSensitive_SignatureDoesNotBypassConsentGate(t *testing.T) {
	tests := []struct {
		name string
		m    Manifest
		want bool
	}{
		{
			name: "git_commit is sensitive when unsigned",
			m:    Manifest{DeclaresTools: []string{"file_read", "git_commit"}},
			want: true,
		},
		{
			name: "git_commit stays sensitive with an unverified signature",
			m: Manifest{
				DeclaresTools: []string{"file_read", "git_commit"},
				Signature:     "not-a-real-signature",
			},
			want: true,
		},
		{
			name: "declared env is sensitive when unsigned",
			m:    Manifest{DeclaresEnv: []string{"GITHUB_TOKEN"}},
			want: true,
		},
		{
			name: "declared env stays sensitive with an unverified signature",
			m: Manifest{
				DeclaresEnv: []string{"GITHUB_TOKEN"},
				Signature:   "sig",
			},
			want: true,
		},
		{
			name: "harmless package is not sensitive",
			m:    Manifest{DeclaresTools: []string{"file_read", "grep"}},
			want: false,
		},
		{
			name: "harmless package with a signature is still not sensitive",
			m:    Manifest{DeclaresTools: []string{"file_read"}, Signature: "sig"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.Sensitive(); got != tc.want {
				t.Fatalf("Sensitive() = %v, want %v (signature=%q)",
					got, tc.want, tc.m.Signature)
			}
		})
	}
}

// TestConsentSummary_NeverClaimsVerified guards the UX half of the same bug:
// the consent summary printed a bare "signed" for an unverified author claim.
func TestConsentSummary_NeverClaimsVerified(t *testing.T) {
	m := Manifest{
		Name:      "evil",
		Version:   "1.0.0",
		Type:      "agent",
		Signature: "not-a-real-signature",
	}
	got := m.ConsentSummary()
	if !strings.Contains(got, "UNVERIFIED") {
		t.Fatalf("consent summary must mark the signature unverified, got:\n%s", got)
	}
}
