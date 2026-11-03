// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testEmbedderID = "fake-bow-64"

// savedCorpus indexes the test corpus, saves it, and returns the index and
// the file path.
func savedCorpus(t *testing.T) (*Flat, string) {
	t.Helper()
	f := indexCorpus(t, fakeEmbedder{dim: 64})
	path := filepath.Join(t.TempDir(), "memory", FileName)
	if err := f.Save(path, testEmbedderID); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return f, path
}

func searchAll(t *testing.T, f *Flat, query string) []Hit {
	t.Helper()
	q, _ := fakeEmbedder{dim: 64}.Embed(context.TODO(), []string{query})
	hits, err := f.Search(q[0], 50, -1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

// TestSaveLoadRoundTrip: a loaded index answers exactly as the saved one did,
// and the file is private (0600) in a private folder (0700).
func TestSaveLoadRoundTrip(t *testing.T) {
	f, path := savedCorpus(t)
	g, err := Load(path, 64, testEmbedderID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.Len() != f.Len() || g.Len() == 0 {
		t.Fatalf("Len: loaded %d, saved %d (want equal, non-zero)", g.Len(), f.Len())
	}
	for _, q := range []string{"bread dough oven", "gateway route budget", "strict policy rules"} {
		if a, b := searchAll(t, f, q), searchAll(t, g, q); !reflect.DeepEqual(a, b) {
			t.Errorf("query %q: loaded index answers differently", q)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
	}
}

// TestLoadAsksForReindex: anything that makes the file unusable as-is yields
// an EMPTY index and ErrReindex — never a partial index.
func TestLoadAsksForReindex(t *testing.T) {
	_, path := savedCorpus(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edit := func(mut func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		mut(m)
		b, _ := json.Marshal(m)
		return b
	}
	firstEntry := func(m map[string]any) map[string]any {
		return m["entries"].([]any)[0].(map[string]any)
	}
	cases := []struct {
		name     string
		content  []byte // nil: file absent
		dim      int
		embedder string
	}{
		{"missing", nil, 64, testEmbedderID},
		{"other-embedder", raw, 64, "another-model"},
		{"other-dim", raw, 32, testEmbedderID},
		{"newer-version", edit(func(m map[string]any) { m["version"] = FormatVersion + 1 }), 64, testEmbedderID},
		{"wrong-format", edit(func(m map[string]any) { m["format"] = "something-else" }), 64, testEmbedderID},
		{"truncated", raw[:len(raw)/2], 64, testEmbedderID},
		{"tampered-text", edit(func(m map[string]any) { firstEntry(m)["text"] = "changed" }), 64, testEmbedderID},
		{"short-vector", edit(func(m map[string]any) { firstEntry(m)["vec"] = "AAAAAA==" }), 64, testEmbedderID},
		{"zero-vector", edit(func(m map[string]any) {
			firstEntry(m)["vec"] = zeroVecB64(64)
		}), 64, testEmbedderID},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), FileName)
		if c.content != nil {
			if err := os.WriteFile(p, c.content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		g, err := Load(p, c.dim, c.embedder)
		if !errors.Is(err, ErrReindex) {
			t.Errorf("%s: err = %v, want ErrReindex", c.name, err)
		}
		if g == nil || g.Len() != 0 || g.Dim() != c.dim {
			t.Errorf("%s: want an empty index of dim %d", c.name, c.dim)
		}
	}
}

// TestSaveIsAtomic: a save that fails after the temp file is written leaves
// the previous file byte-for-byte as it was and no temp file behind.
func TestSaveIsAtomic(t *testing.T) {
	f, path := savedCorpus(t)
	before, _ := os.ReadFile(path)
	if n := f.Delete("kitchen.md"); n == 0 {
		t.Fatalf("fixture: corpus has no kitchen.md")
	}
	saveHook = func(string) error { return errors.New("simulated crash") }
	defer func() { saveHook = nil }()
	if err := f.Save(path, testEmbedderID); err == nil {
		t.Fatalf("Save with failing hook: no error")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("previous file changed by a failed save")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*"))
	if len(left) != 1 {
		t.Errorf("files left in the folder: %d, want 1", len(left))
	}
}

func zeroVecB64(dim int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, 4*dim))
}
