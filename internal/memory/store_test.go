// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	ryshDir := filepath.Join(t.TempDir(), ".rysh")
	s := Open(ryshDir)
	clock := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time {
		clock = clock.Add(time.Minute)
		return clock
	}
	return s, ryshDir
}

func TestAddWritesOneFilePerEntryAndAGitignore(t *testing.T) {
	s, ryshDir := newTestStore(t)
	e, err := s.Add(KindPreference, "prefer table-driven tests", SourceUser, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !validID(e.ID) {
		t.Fatalf("id %q is not a valid entry id", e.ID)
	}
	path := filepath.Join(ryshDir, "memory", "entries", e.ID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("entry file: %v", err)
	}
	if !strings.Contains(string(data), "kind: preference") || !strings.Contains(string(data), "prefer table-driven tests") {
		t.Fatalf("entry file content is not the expected form")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("entry file mode = %v, want 0600", fi.Mode().Perm())
	}
	gi, err := os.ReadFile(filepath.Join(ryshDir, "memory", ".gitignore"))
	if err != nil || strings.TrimSpace(string(gi)) != "*" {
		t.Fatalf(".rysh/memory/.gitignore must exist and ignore everything")
	}
}

func TestAddRefusesCredentialShapedTextAndWritesNothing(t *testing.T) {
	s, ryshDir := newTestStore(t)
	_, err := s.Add(KindFact, "use "+"gh"+"p_"+synthRun(36), SourceAgent, nil)
	if !errors.Is(err, ErrCredentialShaped) {
		t.Fatalf("err = %v, want ErrCredentialShaped", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(ryshDir, "memory", "entries")); len(entries) != 0 {
		t.Fatalf("a refused entry left %d file(s) behind", len(entries))
	}
}

func TestAddValidatesKindSizeAndEmptiness(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Add(Kind("secret"), "x", SourceUser, nil); err == nil {
		t.Fatalf("unknown kind accepted")
	}
	if _, err := s.Add(KindNote, "   ", SourceUser, nil); err == nil {
		t.Fatalf("empty text accepted")
	}
	if _, err := s.Add(KindNote, strings.Repeat("word ", MaxEntryBytes/5+2), SourceUser, nil); err == nil {
		t.Fatalf("over-size text accepted")
	}
}

func TestListOrdersByKindThenNewestAndWithholdsHandEditedSecrets(t *testing.T) {
	s, _ := newTestStore(t)
	n1, _ := s.Add(KindNote, "old note", SourceUser, nil)
	f1, _ := s.Add(KindFact, "the API lives in rysh-server", SourceUser, nil)
	p1, _ := s.Add(KindPreference, "short answers", SourceUser, nil)
	f2, _ := s.Add(KindFact, "go 1.24 is required", SourceAgent, nil)

	// File form: a user hand-writes an entry without a header, and another
	// that holds a credential-shaped value. Both are read; the second is
	// withheld.
	if err := os.WriteFile(filepath.Join(s.Dir(), "handwritten.md"), []byte("deploys go out on Fridays\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := "---\nkind: fact\n---\nthe key is " + "xo" + "xb-" + synthRun(30) + "\n"
	if err := os.WriteFile(filepath.Join(s.Dir(), "pasted.md"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not entries: a bad name, a non-md file and a symlink are ignored.
	_ = os.WriteFile(filepath.Join(s.Dir(), "Bad Name.md"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(s.Dir(), "notes.txt"), []byte("x"), 0o600)
	_ = os.Symlink(filepath.Join(s.Dir(), "handwritten.md"), filepath.Join(s.Dir(), "link.md"))

	list, err := s.List(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []string
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	want := []string{p1.ID, f2.ID, f1.ID, "pasted", "handwritten", n1.ID}
	if !sameSet(ids, want) {
		t.Fatalf("ids = %v, want the set %v", ids, want)
	}
	pos := map[string]int{}
	for i, id := range ids {
		pos[id] = i
	}
	if pos[p1.ID] != 0 || pos[f2.ID] > pos[f1.ID] || pos[n1.ID] < pos[f1.ID] {
		t.Fatalf("order: preferences, then facts newest first, then notes; got %v", ids)
	}
	for _, e := range list {
		switch e.ID {
		case "pasted":
			if e.Withheld != RulePrefix || e.Text != "" {
				t.Fatalf("hand-edited credential entry: withheld=%q, text kept=%v", e.Withheld, e.Text != "")
			}
		case "handwritten":
			if e.Kind != KindNote || e.Source != SourceFile || e.Withheld != "" {
				t.Fatalf("headerless file: kind=%q source=%q withheld=%q", e.Kind, e.Source, e.Withheld)
			}
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func TestGetAndForget(t *testing.T) {
	s, _ := newTestStore(t)
	e, _ := s.Add(KindFact, "rysh-cli is excluded from go.work", SourceUser, nil)
	got, err := s.Get(e.ID, nil)
	if err != nil || got.Text != "rysh-cli is excluded from go.work" {
		t.Fatalf("Get: %v %q", err, got.Text)
	}
	for _, bad := range []string{"../policy", "a/b", "", "UPPER", ".hidden"} {
		if _, err := s.Get(bad, nil); err == nil {
			t.Fatalf("Get(%q) accepted an invalid id", bad)
		}
		if err := s.Forget(bad); err == nil {
			t.Fatalf("Forget(%q) accepted an invalid id", bad)
		}
	}
	if err := s.Forget(e.ID); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := s.Get(e.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Forget, Get err = %v, want ErrNotFound", err)
	}
	if err := s.Forget(e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Forget err = %v, want ErrNotFound", err)
	}
}

func TestForgetAllRemovesOnlyEntries(t *testing.T) {
	s, ryshDir := newTestStore(t)
	_, _ = s.Add(KindFact, "one", SourceUser, nil)
	_, _ = s.Add(KindFact, "two", SourceUser, nil)
	_ = os.WriteFile(filepath.Join(s.Dir(), "notes.txt"), []byte("x"), 0o600)
	n, err := s.ForgetAll()
	if err != nil || n != 2 {
		t.Fatalf("ForgetAll = %d, %v; want 2, nil", n, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "notes.txt")); err != nil {
		t.Fatalf("ForgetAll removed a non-entry file")
	}
	if _, err := os.Stat(filepath.Join(ryshDir, "memory", ".gitignore")); err != nil {
		t.Fatalf("ForgetAll removed the .gitignore")
	}
}

func TestIDsAreUniqueForTheSameText(t *testing.T) {
	s, _ := newTestStore(t)
	a, _ := s.Add(KindNote, "same words", SourceUser, nil)
	b, err := s.Add(KindNote, "same words", SourceUser, nil)
	if err != nil || a.ID == b.ID {
		t.Fatalf("second add: id %q vs %q, err %v", a.ID, b.ID, err)
	}
}

func TestInjectionBlockCapsAndCites(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 30; i++ {
		if _, err := s.Add(KindFact, "fact number "+strings.Repeat("x", 100)+" "+string(rune('a'+i%26)), SourceUser, nil); err != nil {
			t.Fatal(err)
		}
	}
	bad := "---\nkind: preference\n---\npass" + "word=" + "hunter2hunter2\n"
	_ = os.WriteFile(filepath.Join(s.Dir(), "leak.md"), []byte(bad), 0o600)
	list, _ := s.List(nil)

	block := InjectionBlock(list, 1024)
	if block == "" {
		t.Fatalf("empty block")
	}
	if len(block) > 1024 {
		t.Fatalf("block is %d bytes, over the 1024 cap", len(block))
	}
	if !strings.Contains(block, "more memories not shown") {
		t.Fatalf("over-cap block must say how many were left out")
	}
	if strings.Contains(block, "hunter2") || strings.Contains(block, "leak") {
		t.Fatalf("a withheld entry reached the prompt")
	}
	if !strings.Contains(block, "["+list[1].ID+"]") {
		t.Fatalf("injected lines must cite their id")
	}
	if InjectionBlock(nil, 1024) != "" {
		t.Fatalf("no entries must inject nothing")
	}
	if InjectionBlock(list, 0) != "" {
		t.Fatalf("a zero cap must inject nothing")
	}
}

func TestMissingStoreIsEmpty(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), ".rysh"))
	list, err := s.List(nil)
	if err != nil || len(list) != 0 {
		t.Fatalf("missing store: %d entries, err %v", len(list), err)
	}
}
