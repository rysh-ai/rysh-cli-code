package llms

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRef(t *testing.T) {
	p, n, err := ParseRef("openai/gpt55")
	if err != nil || p != "openai" || n != "gpt55" {
		t.Fatalf("ParseRef: %q %q %v", p, n, err)
	}
	for _, bad := range []string{"", "openai", "/x", "x/", "a/b/c", "../x/y", "a/../b", "a b/c"} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted, want error", bad)
		}
	}
}

func TestSeedListAddGet(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.SeedIfEmpty(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	providers, byProvider, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"anthropic", "openai", "grok", "gemini"} {
		if _, ok := byProvider[want]; !ok {
			t.Errorf("seed missing provider %q (got %v)", want, providers)
		}
	}
	// Files live at <rysh-dir>/llms/<provider>/<name>, plain YAML.
	if _, err := os.Stat(filepath.Join(dir, "llms", "anthropic", "fable5")); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	spec, err := s.Get("anthropic", "fable5")
	if err != nil || spec.Model != "claude-fable-5" {
		t.Fatalf("get fable5: %+v %v", spec, err)
	}
	if !spec.Executable() {
		t.Errorf("anthropic model should be executable")
	}
	// openai and gemini became runnable when the executor learned their
	// dialects; grok never did.
	if got, _ := s.Get("openai", "gpt-4o"); !got.Executable() {
		t.Errorf("openai models are runnable — rysh speaks the Responses API")
	}
	if got, _ := s.Get("gemini", "gemini-2.5-flash"); !got.Executable() {
		t.Errorf("gemini models are runnable via the OpenAI-compat surface")
	}
	if got, _ := s.Get("grok", "grok-4"); got.Executable() {
		t.Errorf("grok has no rysh executor — it must stay declaration-only")
	}

	// Seeding again must not duplicate or overwrite.
	if err := s.SeedIfEmpty(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// Add a new declaration; name doubles as model id when omitted.
	if _, err := s.Add(ModelSpec{Provider: "openai", Name: "gpt55"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	spec, err = s.Get("openai", "gpt55")
	if err != nil || spec.Model != "gpt55" || spec.Added == "" {
		t.Fatalf("get gpt55: %+v %v", spec, err)
	}
}
