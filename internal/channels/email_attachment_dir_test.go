// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// E17 M1 (e2): AttachmentDir resolves email.attachment_dir. Set: that
// directory, with ~ expanded; it must already exist and be a directory, and
// must be absolute after expansion — otherwise a clear error, never a silent
// mkdir or fallback (a typo would otherwise scatter mail into an unexpected
// place). Unset: the unchanged default, ~/Downloads else home.

func TestAttachmentDirConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	files := filepath.Join(home, "mail-files")
	if err := os.Mkdir(files, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"~/mail-files", files} {
		got, err := AttachmentDir(in)
		if err != nil || got != files {
			t.Errorf("AttachmentDir(%q) = %q, %v; want %q", in, got, err, files)
		}
	}
	// And a save actually lands there.
	dir, _ := AttachmentDir("~/mail-files")
	p, err := saveAttachmentTo(dir, "a.txt", []byte("x"))
	if err != nil || filepath.Dir(p) != files {
		t.Fatalf("save went to %q (%v), want inside %q", p, err, files)
	}
}

func TestAttachmentDirConfiguredMustExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, "plain-file")
	_ = os.WriteFile(file, []byte("x"), 0o600)

	for _, tc := range []struct{ in, wantErr string }{
		{"~/no-such-dir", "does not exist"},
		{file, "not a directory"},
		{"relative/dir", "absolute"},
	} {
		got, err := AttachmentDir(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "email.attachment_dir") {
			t.Errorf("AttachmentDir(%q) = %q, %v; want an email.attachment_dir error mentioning %q", tc.in, got, err, tc.wantErr)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "no-such-dir")); err == nil {
		t.Error("a missing configured directory was created")
	}
}

func TestAttachmentDirDefaultUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, err := AttachmentDir(""); err != nil || got != home {
		t.Fatalf("no Downloads: AttachmentDir(\"\") = %q, %v; want home %q", got, err, home)
	}
	dl := filepath.Join(home, "Downloads")
	_ = os.Mkdir(dl, 0o700)
	if got, err := AttachmentDir(""); err != nil || got != dl {
		t.Fatalf("with Downloads: AttachmentDir(\"\") = %q, %v; want %q", got, err, dl)
	}
}
