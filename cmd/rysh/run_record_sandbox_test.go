// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// Design 066 S3: the run record says whether the run was sandboxed.
func TestBuildRunRecord_NotesSandbox(t *testing.T) {
	in := testRecordInput(t, minimalAudit(), nil)
	b, err := buildRunRecord(in)
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeRecord(t, b.Record); r.Sandboxed != "none" {
		t.Fatalf("unsandboxed run: sandboxed = %q", r.Sandboxed)
	}

	in.Sandbox = "macos/box"
	b, err = buildRunRecord(in)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeRecord(t, b.Record)
	if r.Sandboxed != "macos/box" {
		t.Fatalf("sandboxed = %q", r.Sandboxed)
	}
	if !strings.Contains(strings.Join(r.Limits, "\n"), "self-reported") {
		t.Fatal("a sandboxed record must say the marker is self-reported")
	}
}
