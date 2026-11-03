// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupDirName is the folder under .rysh/ that holds one backup per import.
const BackupDirName = "import-backup"

const (
	manifestName = "manifest.json"
	undoneMarker = "undone"
)

// Change is one file an import writes, relative to the project root. Only
// paths under .rysh/ are accepted.
type Change struct {
	Path string
	Data []byte
}

// ManifestFile records one written file: whether it existed before (its old
// bytes are in before/<path>) and the bytes the import wrote (after/<path>).
type ManifestFile struct {
	Path    string      `json:"path"`
	Existed bool        `json:"existed"`
	Mode    fs.FileMode `json:"mode,omitempty"`
}

// Manifest describes one import's backup.
type Manifest struct {
	Source  string         `json:"source"`
	Created string         `json:"created"`
	Files   []ManifestFile `json:"files"`
	Dir     string         `json:"-"` // the backup folder, relative to the project root
}

// checkPath accepts only clean, relative paths inside .rysh/ and outside the
// backup folder itself.
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || filepath.IsAbs(p) || path.Clean(p) != p || strings.Contains(p, "\\") {
		return fmt.Errorf("refusing to write %q: not a clean relative path", p)
	}
	if !strings.HasPrefix(p, ".rysh/") || strings.HasPrefix(p, ".rysh/"+BackupDirName+"/") {
		return fmt.Errorf("refusing to write %q: an import writes only under .rysh/", p)
	}
	return nil
}

// Apply backs up every file it is about to change, then writes changes. The
// manifest is saved before the first write, so even an apply that fails half
// way can be undone.
func Apply(root, source string, changes []Change, now time.Time) (*Manifest, error) {
	if len(changes) == 0 {
		return nil, errors.New("nothing to import")
	}
	for _, c := range changes {
		if err := checkPath(c.Path); err != nil {
			return nil, err
		}
	}
	base := filepath.Join(root, ".rysh", BackupDirName)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	// Backups can hold copies of the user's own config; keep them out of git.
	if err := os.WriteFile(filepath.Join(base, ".gitignore"), []byte("*\n"), 0o600); err != nil {
		return nil, err
	}
	name := now.UTC().Format("20060102T150405Z") + "-" + slug(source)
	dir := filepath.Join(base, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(base, fmt.Sprintf("%s-%d", name, i))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, dir)
	m := &Manifest{Source: source, Created: now.UTC().Format(time.RFC3339), Dir: filepath.ToSlash(rel)}
	for _, c := range changes {
		target := filepath.Join(root, filepath.FromSlash(c.Path))
		mf := ManifestFile{Path: c.Path}
		if fi, err := os.Stat(target); err == nil {
			if !fi.Mode().IsRegular() {
				return nil, fmt.Errorf("refusing to replace %s: not a regular file", c.Path)
			}
			old, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			if err := writeFile(filepath.Join(dir, "before", filepath.FromSlash(c.Path)), old, 0o600); err != nil {
				return nil, err
			}
			mf.Existed, mf.Mode = true, fi.Mode().Perm()
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := writeFile(filepath.Join(dir, "after", filepath.FromSlash(c.Path)), c.Data, 0o600); err != nil {
			return nil, err
		}
		m.Files = append(m.Files, mf)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, manifestName), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	for i, c := range changes {
		mode := fs.FileMode(0o644)
		if m.Files[i].Existed {
			mode = m.Files[i].Mode
		}
		if err := writeFile(filepath.Join(root, filepath.FromSlash(c.Path)), c.Data, mode); err != nil {
			return m, fmt.Errorf("write %s: %w (undo with rysh import --undo)", c.Path, err)
		}
	}
	return m, nil
}

// writeFile writes atomically (temp file + rename), creating parents.
func writeFile(target string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".import-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(name, mode)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, target)
}

// lastImport finds the newest backup not yet undone.
func lastImport(root string) (*Manifest, string, error) {
	base := filepath.Join(root, ".rysh", BackupDirName)
	entries, err := os.ReadDir(base)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		dir := filepath.Join(base, n)
		if _, err := os.Stat(filepath.Join(dir, undoneMarker)); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, manifestName))
		if err != nil {
			continue // an apply that died before its manifest wrote nothing
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, "", fmt.Errorf("backup %s: %w", n, err)
		}
		rel, _ := filepath.Rel(root, dir)
		m.Dir = filepath.ToSlash(rel)
		return &m, dir, nil
	}
	return nil, "", errors.New("no import to undo")
}

// Undo restores the last import: replaced files get their old bytes back and
// created files are removed. It refuses, changing nothing, when a file was
// edited after the import; UndoForce restores anyway.
func Undo(root string) (*Manifest, error) { return undo(root, false) }

// UndoForce is Undo without the edited-since check.
func UndoForce(root string) (*Manifest, error) { return undo(root, true) }

func undo(root string, force bool) (*Manifest, error) {
	m, dir, err := lastImport(root)
	if err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		if err := checkPath(f.Path); err != nil {
			return nil, fmt.Errorf("backup %s: %w", m.Dir, err)
		}
	}
	if !force {
		var changed []string
		for _, f := range m.Files {
			want, err := os.ReadFile(filepath.Join(dir, "after", filepath.FromSlash(f.Path)))
			if err != nil {
				return nil, err
			}
			got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if err != nil || !bytes.Equal(got, want) {
				changed = append(changed, f.Path)
			}
		}
		if len(changed) > 0 {
			return nil, fmt.Errorf("files changed since the %s import: %s (nothing restored; rysh import --undo --force restores anyway)", m.Source, strings.Join(changed, ", "))
		}
	}
	for _, f := range m.Files {
		target := filepath.Join(root, filepath.FromSlash(f.Path))
		if !f.Existed {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}
		old, err := os.ReadFile(filepath.Join(dir, "before", filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, err
		}
		if err := writeFile(target, old, f.Mode); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, undoneMarker), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return m, nil
}
