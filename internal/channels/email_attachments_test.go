// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// E17 M1 attachments: EmailDetail.Attachments existed on the wire but nothing
// ever filled it, and nothing could save one. parseAttachments walks the MIME
// tree; saveAttachmentTo writes one into a directory — with a name taken from
// an email an attacker can send, so it must never leave that directory or
// overwrite anything.

// attachmentBody is a multipart/mixed body as parseEmailResponse returns it
// (lines re-joined with "\n").
const attachmentBody = `--OUTER
Content-Type: multipart/alternative; boundary="INNER"

--INNER
Content-Type: text/plain

hello body
--INNER
Content-Type: text/html

<p>hello</p>
--INNER--
--OUTER
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--OUTER
Content-Type: text/plain; charset=utf-8
Content-Disposition: attachment; filename*=UTF-8''notes%20%C3%BC.txt
Content-Transfer-Encoding: quoted-printable

caf=C3=A9
--OUTER
Content-Type: image/png
Content-Disposition: inline; filename="=?UTF-8?B?bG9nby5wbmc=?="
Content-Transfer-Encoding: base64

iVBORw0K
--OUTER--
`

const attachmentCT = `multipart/mixed; boundary="OUTER"`

func TestParseAttachments(t *testing.T) {
	got := parseAttachments(attachmentBody, attachmentCT)
	want := []struct {
		name, ct string
		data     []byte
	}{
		{"report.pdf", "application/pdf", []byte("%PDF-1.4\n")},
		{"notes ü.txt", "text/plain", []byte("café")},
		{"logo.png", "image/png", []byte("\x89PNG\r\n")},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d attachments %+v, want %d (the text/html alternative is a body, not an attachment)", len(got), got, len(want))
	}
	for i, w := range want {
		a := got[i]
		if a.Info.Filename != w.name || a.Info.ContentType != w.ct {
			t.Errorf("attachment[%d] = %q %q, want %q %q", i, a.Info.Filename, a.Info.ContentType, w.name, w.ct)
		}
		if !bytes.Equal(a.Data, w.data) || a.Info.Size != len(w.data) {
			t.Errorf("attachment[%d] data = %q (size %d), want %q decoded", i, a.Data, a.Info.Size, w.data)
		}
	}
	// The text body is still extracted from the same message.
	if body, _ := extractTextBody(attachmentBody, attachmentCT); !strings.Contains(body, "hello body") {
		t.Errorf("text body = %q, want the text/plain part", body)
	}
}

func TestParseAttachmentsSinglePart(t *testing.T) {
	if got := parseAttachments("just text", "text/plain"); len(got) != 0 {
		t.Fatalf("a single-part text mail has attachments: %+v", got)
	}
}

func TestSaveAttachmentToStaysInDirAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()

	p1, err := saveAttachmentTo(dir, "report.pdf", []byte("one"))
	if err != nil || p1 != filepath.Join(dir, "report.pdf") {
		t.Fatalf("first save = %q, %v", p1, err)
	}
	if fi, _ := os.Stat(p1); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("saved file mode = %v, want 0600 (mail content is private)", fi.Mode().Perm())
	}
	p2, err := saveAttachmentTo(dir, "report.pdf", []byte("two"))
	if err != nil || p2 != filepath.Join(dir, "report (1).pdf") {
		t.Fatalf("second save = %q, %v, want \"report (1).pdf\"", p2, err)
	}
	if b, _ := os.ReadFile(p1); string(b) != "one" {
		t.Fatalf("second save overwrote the first: %q", b)
	}

	for _, tc := range []struct{ in, want string }{
		{"../../etc/passwd", "passwd"},
		{`..\..\win.ini`, "win.ini"},
		{"/abs/path/x.txt", "x.txt"},
		{"a\x00b\nc.txt", "abc.txt"},
		{"..", "attachment"},
		{"", "attachment"},
	} {
		d := t.TempDir() // fresh dir: ".." and "" both sanitize to "attachment"
		p, err := saveAttachmentTo(d, tc.in, []byte("x"))
		if err != nil {
			t.Fatalf("save %q: %v", tc.in, err)
		}
		if filepath.Dir(p) != d {
			t.Errorf("save %q wrote %q, outside %q", tc.in, p, d)
		}
		if base := filepath.Base(p); base != tc.want {
			t.Errorf("save %q named the file %q, want %q", tc.in, base, tc.want)
		}
	}
}
