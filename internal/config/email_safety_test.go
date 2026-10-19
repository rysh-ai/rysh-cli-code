// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// E17 WO-7 (v), founder ruling U-6/H-5: the opt-in read-only safety guard is
// configured under email: — safety (read_only; default off), subject_tag (the
// only mail the guard lets rysh touch or send) and audit_log (where every IMAP
// command and every fetched UID/Message-ID is recorded).
func TestLoadEmailSafety(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	yaml := "email:\n  safety: read_only\n  subject_tag: RYSH-TEST-M1\n  audit_log: ~/m1-audit.jsonl\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadFrom(path).Email
	if got.Safety != "read_only" || got.SubjectTag != "RYSH-TEST-M1" || got.AuditLog != "~/m1-audit.jsonl" {
		t.Fatalf("Email safety = %q tag %q audit %q, want read_only / RYSH-TEST-M1 / ~/m1-audit.jsonl", got.Safety, got.SubjectTag, got.AuditLog)
	}
}

func TestLoadEmailSafetyDefaultsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("email:\n  imap_host: imap.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(path).Email; got.Safety != "" || got.SubjectTag != "" {
		t.Fatalf("safety must default off; got %q / %q", got.Safety, got.SubjectTag)
	}
}

// E17 WO-11, root-0320: email.allow_from is the guard's sender allow-list.
func TestLoadEmailAllowFrom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	yaml := "email:\n  safety: read_only\n  subject_tag: RYSH-TEST-M1\n  allow_from: [m1-run@example.test, other@example.test]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadFrom(path).Email.AllowFrom
	if len(got) != 2 || got[0] != "m1-run@example.test" || got[1] != "other@example.test" {
		t.Fatalf("Email.AllowFrom = %q, want [m1-run@example.test other@example.test]", got)
	}
}
