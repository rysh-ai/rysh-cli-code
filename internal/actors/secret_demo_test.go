// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
	"unicode"
)

// TestFakeSecretShape verifies the demo decoy keeps the value's shape (length,
// punctuation, character classes), never equals it, and is deterministic.
func TestFakeSecretShape(t *testing.T) {
	for _, v := range []string{"sk_live_ABC123xyz", "xoxb-1234-abcd-EFGH", "hunter2", "a", "p@ss.w0rd/Z"} {
		f := fakeSecret(v)
		if f == v {
			t.Errorf("fakeSecret(%q) returned the real value", v)
		}
		if f != fakeSecret(v) {
			t.Errorf("fakeSecret(%q) is not deterministic", v)
		}
		fr, vr := []rune(f), []rune(v)
		if len(fr) != len(vr) {
			t.Fatalf("fakeSecret(%q) = %q: length changed", v, f)
		}
		for i := range vr {
			same := (unicode.IsLower(vr[i]) && unicode.IsLower(fr[i])) ||
				(unicode.IsUpper(vr[i]) && unicode.IsUpper(fr[i])) ||
				(unicode.IsDigit(vr[i]) && unicode.IsDigit(fr[i])) ||
				vr[i] == fr[i]
			if !same {
				t.Errorf("fakeSecret(%q) = %q: class changed at %d", v, f, i)
			}
			if (unicode.IsLetter(vr[i]) || unicode.IsDigit(vr[i])) && vr[i] == fr[i] {
				t.Errorf("fakeSecret(%q) = %q: alnum kept at %d", v, f, i)
			}
		}
	}
	if fp := secretFingerprint("x"); !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+16 {
		t.Errorf("secretFingerprint = %q", fp)
	}
}
