// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// E17 M1 (e2): email.attachment_dir overrides where saved attachments go.
func TestLoadEmailAttachmentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("email:\n  attachment_dir: ~/mail-files\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(path).Email.AttachmentDir; got != "~/mail-files" {
		t.Fatalf("Email.AttachmentDir = %q, want the raw configured value \"~/mail-files\"", got)
	}
}
