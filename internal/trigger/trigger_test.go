// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []struct{ name, mode, input string }{
		{"gh-push", "", "@reviewer review the latest push"},
		{"nightly_1", "prompt", "summarise yesterday's merges"},
		{"N", "", "write the release notes"}, // bare text defaults to prompt
	}
	for _, c := range ok {
		if err := Validate(c.name, c.mode, c.input); err != nil {
			t.Errorf("Validate(%q, %q, %q) = %v, want nil", c.name, c.mode, c.input, err)
		}
	}
	bad := []struct{ name, mode, input, why string }{
		{"", "", "@a hi", "empty name"},
		{"../etc", "", "@a hi", "path in name"},
		{"a b", "", "@a hi", "space in name"},
		{strings.Repeat("x", 65), "", "@a hi", "name too long"},
		{"t", "", "  ", "empty input"},
		{"t", "shell", "echo hi", "shell mode"},
		{"t", "rysh", "secret list", "rysh mode"},
		{"t", "", "##secret list", "## command"},
		{"t", "", "####new grid", "#### relay"},
		{"t", "", "#deploy", "user command"},
		{"t", "", "@@bot stop", "@@ control"},
		{"t", "prompt", "##cron rm x", "## in prompt mode"},
	}
	for _, c := range bad {
		if err := Validate(c.name, c.mode, c.input); err == nil {
			t.Errorf("Validate accepted %s (%q, %q, %q)", c.why, c.name, c.mode, c.input)
		}
	}
}

func TestEffectiveMode(t *testing.T) {
	if got := EffectiveMode("", "@a hi"); got != "rysh" {
		t.Errorf("@agent input mode = %q, want rysh (prefix-dispatched)", got)
	}
	if got := EffectiveMode("", "hello"); got != "prompt" {
		t.Errorf("bare input mode = %q, want prompt", got)
	}
}

func TestSignVerify(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewSecret()
	body := []byte(`{"ref":"refs/heads/main"}`)
	sig := Sign(secret, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("signature header lacks the sha256= scheme")
	}
	if !Verify(secret, body, sig) {
		t.Fatal("a correct signature did not verify")
	}
	for name, h := range map[string]string{
		"empty":        "",
		"no scheme":    strings.TrimPrefix(sig, "sha256="),
		"wrong scheme": "sha1=" + strings.TrimPrefix(sig, "sha256="),
		"not hex":      "sha256=zz",
		"other secret": Sign(other, body),
		"other body":   Sign(secret, []byte(`{}`)),
	} {
		if Verify(secret, body, h) {
			t.Errorf("Verify accepted %s", name)
		}
	}
	if Verify(nil, body, sig) || Verify([]byte{}, body, sig) {
		t.Error("Verify accepted an empty secret")
	}
}

func TestSecretFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "triggers")
	if err := WriteSecret(dir, "gh-push"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "gh-push.secret"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("secret dir mode = %o, want 700", dst.Mode().Perm())
	}
	first, ok := ReadSecret(dir, "gh-push")
	if !ok || len(first) == 0 {
		t.Fatal("written secret could not be read back")
	}
	if err := WriteSecret(dir, "gh-push"); err != nil { // rotate
		t.Fatal(err)
	}
	second, _ := ReadSecret(dir, "gh-push")
	if string(first) == string(second) {
		t.Fatal("rotation kept the old secret")
	}
	if _, ok := ReadSecret(dir, "../gh-push"); ok {
		t.Fatal("ReadSecret followed a path-shaped name")
	}
	if _, ok := ReadSecret(dir, "missing"); ok {
		t.Fatal("ReadSecret found a secret that was never written")
	}
	if err := RemoveSecret(dir, "gh-push"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadSecret(dir, "gh-push"); ok {
		t.Fatal("secret still readable after RemoveSecret")
	}
}

func TestDeliveryRing(t *testing.T) {
	var r deliveryRing
	if r.seen("a") {
		t.Fatal("empty ring reported a delivery as seen")
	}
	r.add("a")
	if !r.seen("a") {
		t.Fatal("added delivery not seen")
	}
	for i := 0; i < maxDeliveries; i++ {
		r.add(strings.Repeat("x", i+1))
	}
	if r.seen("a") {
		t.Fatal("ring did not evict its oldest delivery")
	}
}

// LEAD-CHANGES (1): a project that tracks .rysh/ must never commit a trigger
// secret, so the secrets dir carries the same `*` / `!.gitignore` file the
// other secret stores write.
func TestSecretDirIsGitignored(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "triggers")
	if err := WriteSecret(dir, "gh-push"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("no .gitignore in the secrets dir: %v", err)
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	if !lines["*"] || !lines["!.gitignore"] {
		t.Fatalf(".gitignore does not ignore everything but itself: %q", b)
	}
}
