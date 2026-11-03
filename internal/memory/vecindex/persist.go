// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// S2 (design 010 §3): the index persists as one file under the workspace
// memory folder (<RyshDir>/memory/, already git-ignored by the entries store).
// The file is a cache of the sources, never the truth: anything that makes it
// unusable as-is — absent, a different format version, embedder or dimension,
// truncated, tampered — loads as an EMPTY index with ErrReindex, and the
// caller rebuilds from the sources. A partial index is never returned.

// FileName is the index file under the workspace memory folder.
const FileName = "vecindex.json"

// FormatVersion is the on-disk format version. A file of any other version is
// rebuilt, not migrated.
const FormatVersion = 1

const formatName = "rysh-vecindex"

// ErrReindex: the file cannot be used as-is; rebuild the index from its
// sources. Load returns it wrapped with the reason.
var ErrReindex = errors.New("vecindex: reindex needed")

// saveHook, when set (tests only), runs after the temp file is written and
// synced, before it replaces the previous file; an error aborts the save.
var saveHook func(tmp string) error

type fileHeader struct {
	Format   string      `json:"format"`
	Version  int         `json:"version"`
	Dim      int         `json:"dim"`
	Embedder string      `json:"embedder"`
	Entries  []fileEntry `json:"entries"`
}

type fileEntry struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Heading string `json:"heading,omitempty"`
	Text    string `json:"text"`
	// Vec is the unit vector, little-endian float32, base64.
	Vec string `json:"vec"`
}

// Save writes the index to path, crash-safe: a temp file in the same folder is
// written and synced, then renamed over the previous file, so a reader sees
// the old file or the new one, never a torn write. embedder names the model
// the vectors came from; Load rebuilds when it differs. The folder is created
// 0700 and the file is 0600.
func (f *Flat) Save(path, embedder string) error {
	f.mu.RLock()
	h := fileHeader{Format: formatName, Version: FormatVersion, Dim: f.dim, Embedder: embedder,
		Entries: make([]fileEntry, len(f.meta))}
	buf := make([]byte, 4*f.dim)
	for i, m := range f.meta {
		for j, x := range f.vecs[i*f.dim : (i+1)*f.dim] {
			binary.LittleEndian.PutUint32(buf[4*j:], math.Float32bits(x))
		}
		h.Entries[i] = fileEntry{Source: m.source, ID: m.id, Heading: m.heading, Text: m.text,
			Vec: base64.StdEncoding.EncodeToString(buf)}
	}
	f.mu.RUnlock()
	// Slot order is not observable; sort so the same index gives the same bytes.
	sort.Slice(h.Entries, func(i, j int) bool {
		a, b := h.Entries[i], h.Entries[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.ID < b.ID
	})
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".vecindex-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if saveHook != nil {
		if err := saveHook(tmpName); err != nil {
			os.Remove(tmpName)
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Make the rename itself durable. Best effort: not every platform can
	// sync a directory.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Load reads the index saved at path for vectors of dimension dim from the
// named embedder. It always returns a usable index: on any reason to rebuild
// it is empty and the error wraps ErrReindex.
func Load(path string, dim int, embedder string) (*Flat, error) {
	empty, err := New(dim)
	if err != nil {
		return nil, err
	}
	reindex := func(why string) (*Flat, error) {
		return empty, fmt.Errorf("%w: %s", ErrReindex, why)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return reindex("no index file")
		}
		return reindex("index file unreadable")
	}
	var h fileHeader
	if err := json.Unmarshal(data, &h); err != nil {
		return reindex("index file is not valid")
	}
	switch {
	case h.Format != formatName:
		return reindex("not an index file")
	case h.Version != FormatVersion:
		return reindex(fmt.Sprintf("format version %d, want %d", h.Version, FormatVersion))
	case h.Dim != dim:
		return reindex(fmt.Sprintf("dimension %d, want %d", h.Dim, dim))
	case h.Embedder != embedder:
		return reindex("built with another embedder")
	}

	g, _ := New(dim)
	for i, e := range h.Entries {
		raw, err := base64.StdEncoding.DecodeString(e.Vec)
		if err != nil || len(raw) != 4*dim {
			return reindex(fmt.Sprintf("entry %d: bad vector", i))
		}
		if e.Source == "" || e.ID != ChunkID(e.Source, e.Text) {
			return reindex(fmt.Sprintf("entry %d: id does not match its text", i))
		}
		if _, dup := g.slots[e.Source][e.ID]; dup {
			return reindex(fmt.Sprintf("entry %d: duplicate chunk", i))
		}
		v := make([]float32, dim)
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*j:]))
		}
		u, err := g.unit(v)
		if err != nil {
			return reindex(fmt.Sprintf("entry %d: %v", i, err))
		}
		if g.slots[e.Source] == nil {
			g.slots[e.Source] = map[string]int{}
		}
		g.slots[e.Source][e.ID] = len(g.meta)
		g.meta = append(g.meta, entry{source: e.Source, id: e.ID, heading: e.Heading, text: e.Text})
		g.vecs = append(g.vecs, u...)
	}
	return g, nil
}
