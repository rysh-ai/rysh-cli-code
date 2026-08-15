// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- Path sandbox tests ---

func TestResolveSandboxedPath_TraversalEscape(t *testing.T) {
	root := t.TempDir()
	// A ".." that climbs above root must be denied when allowAbsolute is false.
	_, _, code, _ := resolveSandboxedPath(root, "../../etc/passwd", false)
	if code != "denied" {
		t.Fatalf("expected denied for .. escape, got %q", code)
	}
}

func TestResolveSandboxedPath_WithinRootOK(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	abs, rel, code, msg := resolveSandboxedPath(root, "sub", false)
	if code != "" {
		t.Fatalf("expected success, got code=%q msg=%q", code, msg)
	}
	if rel != "sub" {
		t.Fatalf("rel = %q, want %q", rel, "sub")
	}
	// EvalSymlinks may prepend /private on macOS; just require the basename match.
	if filepath.Base(abs) != "sub" {
		t.Fatalf("abs basename = %q, want sub", filepath.Base(abs))
	}
}

func TestResolveSandboxedPath_AbsoluteDisallowed(t *testing.T) {
	root := t.TempDir()
	_, _, code, _ := resolveSandboxedPath(root, "/etc/hosts", false)
	if code != "denied" {
		t.Fatalf("expected denied for absolute path when disallowed, got %q", code)
	}
}

func TestResolveSandboxedPath_AbsoluteAllowed(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	target := filepath.Join(other, "f.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, code, msg := resolveSandboxedPath(root, target, true)
	if code != "" {
		t.Fatalf("expected absolute path allowed, got code=%q msg=%q", code, msg)
	}
}

func TestResolveSandboxedPath_SymlinkEscapeDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Accessing through the symlink resolves outside root → denied.
	_, _, code, _ := resolveSandboxedPath(root, "escape/secret.txt", false)
	if code != "denied" {
		t.Fatalf("expected denied for out-of-root symlink, got %q", code)
	}
}

func TestWithinRoot(t *testing.T) {
	root := "/home/user/project"
	cases := []struct {
		target string
		want   bool
	}{
		{"/home/user/project", true},
		{"/home/user/project/src", true},
		{"/home/user/project/src/main.go", true},
		{"/home/user", false},
		{"/home/user/projectile", false}, // prefix but not a child
		{"/etc/passwd", false},
	}
	for _, c := range cases {
		if got := withinRoot(root, filepath.Clean(c.target)); got != c.want {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", root, c.target, got, c.want)
		}
	}
}

// --- Content classification tests ---

func TestClassifyFile(t *testing.T) {
	dir := t.TempDir()

	// Text by extension.
	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cls, _ := classifyFile(goFile, "main.go"); cls != "text" {
		t.Errorf(".go classified as %q, want text", cls)
	}

	// Image by extension + PNG magic bytes.
	pngFile := filepath.Join(dir, "logo.png")
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	if err := os.WriteFile(pngFile, png, 0o644); err != nil {
		t.Fatal(err)
	}
	if cls, mime := classifyFile(pngFile, "logo.png"); cls != "image" || mime != "image/png" {
		t.Errorf("png classified as %q/%q, want image/image/png", cls, mime)
	}

	// Unsupported binary.
	binFile := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(binFile, []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	if cls, _ := classifyFile(binFile, "data.bin"); cls != "unsupported" {
		t.Errorf("binary classified as %q, want unsupported", cls)
	}

	// Extensionless text by sniffing.
	plain := filepath.Join(dir, "notes")
	if err := os.WriteFile(plain, []byte("just some plain text here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cls, _ := classifyFile(plain, "notes"); cls != "text" {
		t.Errorf("plain text sniffed as %q, want text", cls)
	}
}

func TestIsProbablyText(t *testing.T) {
	if !isProbablyText([]byte("hello world\n\ttabbed")) {
		t.Error("ascii text should be text")
	}
	if isProbablyText([]byte{0x00, 0x01, 0x02}) {
		t.Error("NUL bytes should be binary")
	}
}

// --- Chunked read offset/eof boundary tests ---

func TestFSReadChunkBoundaries(t *testing.T) {
	// Verify the offset/length/eof math against fsMaxTextChunk using a small
	// helper that mirrors fsRead's clamping logic, so we test the boundaries
	// without standing up NATS.
	total := int64(fsMaxTextChunk) + 100

	type res struct {
		offset, length int64
		eof            bool
	}
	clamp := func(reqOffset, reqLength int64) res {
		offset := reqOffset
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		length := reqLength
		if length <= 0 || length > fsMaxTextChunk {
			length = fsMaxTextChunk
		}
		if offset+length > total {
			length = total - offset
		}
		return res{offset, length, offset+length >= total}
	}

	// First chunk: full chunk, not eof.
	r := clamp(0, 0)
	if r.length != fsMaxTextChunk || r.eof {
		t.Fatalf("first chunk = %+v, want length=%d eof=false", r, fsMaxTextChunk)
	}
	// Second chunk: remaining 100 bytes, eof.
	r = clamp(fsMaxTextChunk, 0)
	if r.length != 100 || !r.eof {
		t.Fatalf("second chunk = %+v, want length=100 eof=true", r)
	}
	// Offset past end: zero length, eof.
	r = clamp(total+50, 0)
	if r.offset != total || r.length != 0 || !r.eof {
		t.Fatalf("past-end chunk = %+v, want offset=%d length=0 eof=true", r, total)
	}
	// Requested length larger than cap is clamped to cap.
	r = clamp(0, fsMaxTextChunk*4)
	if r.length != fsMaxTextChunk {
		t.Fatalf("oversized request length = %d, want %d", r.length, fsMaxTextChunk)
	}
}

// --- Secret redaction tests ---

func TestRedactSecrets(t *testing.T) {
	in := []byte("HOST=db.example.com\nDB_PASSWORD=hunter2\napi_key: sk-abcdef\nplain line\n")
	out, changed := redactSecrets(in)
	if !changed {
		t.Fatal("expected redaction to change content")
	}
	s := string(out)
	if strings.Contains(s, "hunter2") {
		t.Error("password value leaked")
	}
	if strings.Contains(s, "sk-abcdef") {
		t.Error("token value leaked")
	}
	if !strings.Contains(s, "HOST=db.example.com") {
		t.Error("non-sensitive line should be preserved")
	}

	clean := []byte("just\nplain\ntext\n")
	if _, changed := redactSecrets(clean); changed {
		t.Error("clean content should not be marked redacted")
	}
}

// --- Shared root folder (pinned browse root) tests ---

func TestResolvePaneRoot_StartupDirFallback(t *testing.T) {
	dir := t.TempDir()
	// shellPID 0 skips the /proc lookups, so resolution falls to the startup dir.
	if got := resolvePaneRoot(0, dir); got != dir {
		t.Errorf("resolvePaneRoot(0, %q) = %q, want %q", dir, got, dir)
	}
	// A non-existent startup dir is ignored; resolution falls back to the daemon
	// cwd (non-empty), never the bogus dir.
	if got := resolvePaneRoot(0, filepath.Join(dir, "does-not-exist")); got == filepath.Join(dir, "does-not-exist") {
		t.Errorf("resolvePaneRoot returned a non-existent startup dir %q", got)
	}
}

func TestResolveBrowseRoot_PinnedSharedRootWins(t *testing.T) {
	dir := t.TempDir()
	// With a valid pinned root, resolveBrowseRoot returns it directly without
	// touching the target pane (so a nil publisher is fine here) — every target
	// pane of the share resolves to the same root.
	u := &UpstreamShareActor{sharedRootFolder: dir}
	root, code, msg := u.resolveBrowseRoot("any-pane-id")
	if code != "" {
		t.Fatalf("resolveBrowseRoot error: %s (%s)", code, msg)
	}
	if root != dir {
		t.Errorf("resolveBrowseRoot root = %q, want pinned %q", root, dir)
	}
}

// TestProcCwd_SelfMatchesGetwd verifies live-cwd resolution works on the host
// platform (Linux /proc or macOS lsof): the current process's resolved cwd must
// match os.Getwd. This is the resolution that makes a shared pane's working
// directory the mobile browse root.
func TestProcCwd_SelfMatchesGetwd(t *testing.T) {
	got := procCwd(os.Getpid())
	if got == "" {
		t.Skip("procCwd unavailable here (no /proc and no lsof)")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	g, _ := filepath.EvalSymlinks(got)
	w, _ := filepath.EvalSymlinks(wd)
	if g != w {
		t.Errorf("procCwd(self) = %q (->%q), want cwd %q (->%q)", got, g, wd, w)
	}
}

// TestResolvePaneRootWithSource_LiveCwd verifies that resolvePaneRootWithSource
// resolves the *live* working directory of a real process (source "live") on the
// host platform — i.e. it actually reaches procCwd and is not gated to Linux.
// This is the regression guard for the macOS bug where the live-cwd lookup was
// skipped and the root fell back to the daemon dir (home).
func TestResolvePaneRootWithSource_LiveCwd(t *testing.T) {
	root, source := resolvePaneRootWithSource(os.Getpid(), "")
	if source != "live" {
		t.Skipf("live cwd unavailable here (source=%q); environment lacks /proc and lsof", source)
	}
	wd, _ := os.Getwd()
	g, _ := filepath.EvalSymlinks(root)
	w, _ := filepath.EvalSymlinks(wd)
	if g != w {
		t.Errorf("resolvePaneRootWithSource live root = %q (->%q), want cwd %q (->%q)", root, g, wd, w)
	}
}

// TestResolvePaneRootWithSource_StartupFallback verifies the "startup" source is
// reported when no live cwd is available (shellPID 0) but a startup dir exists.
func TestResolvePaneRootWithSource_StartupFallback(t *testing.T) {
	dir := t.TempDir()
	root, source := resolvePaneRootWithSource(0, dir)
	if root != dir || source != "startup" {
		t.Errorf("resolvePaneRootWithSource(0,%q) = (%q,%q), want (%q,\"startup\")", dir, root, source, dir)
	}
}
