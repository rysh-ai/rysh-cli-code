package actors

// Phase 4 (bash-shell-mode): OSC 7 cwd scanner tests.

import "testing"

func TestOSC7SingleChunk(t *testing.T) {
	var s osc7Scanner
	path, ok := s.feed([]byte("noise\x1b]7;file://host/Users/me/proj\x07more"))
	if !ok || path != "/Users/me/proj" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
}

func TestOSC7STTerminator(t *testing.T) {
	var s osc7Scanner
	path, ok := s.feed([]byte("\x1b]7;file://h/tmp\x1b\\"))
	if !ok || path != "/tmp" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
}

func TestOSC7LastWins(t *testing.T) {
	var s osc7Scanner
	path, ok := s.feed([]byte("\x1b]7;file://h/a\x07\x1b]7;file://h/b\x07"))
	if !ok || path != "/b" {
		t.Fatalf("got %q ok=%v, want /b", path, ok)
	}
}

func TestOSC7SplitAcrossChunks(t *testing.T) {
	var s osc7Scanner
	if _, ok := s.feed([]byte("out\x1b]7;file://host/Users/me/lon")); ok {
		t.Fatal("incomplete sequence must not report")
	}
	path, ok := s.feed([]byte("g/dir\x07rest"))
	if !ok || path != "/Users/me/long/dir" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
}

func TestOSC7SplitInsidePrefix(t *testing.T) {
	var s osc7Scanner
	if _, ok := s.feed([]byte("abc\x1b]7")); ok {
		t.Fatal("prefix fragment must not report")
	}
	path, ok := s.feed([]byte(";file:///x\x07"))
	if !ok || path != "/x" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
}

func TestOSC7PercentDecoding(t *testing.T) {
	var s osc7Scanner
	path, ok := s.feed([]byte("\x1b]7;file://h/a%20dir/b\x07"))
	if !ok || path != "/a dir/b" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
}

func TestOSC7IgnoresNonFileURL(t *testing.T) {
	var s osc7Scanner
	if _, ok := s.feed([]byte("\x1b]7;http://example.com/x\x07")); ok {
		t.Fatal("non-file URL must be ignored")
	}
	// Other OSC sequences don't confuse the scanner.
	if _, ok := s.feed([]byte("\x1b]0;title\x07plain")); ok {
		t.Fatal("OSC 0 must be ignored")
	}
}

func TestOSC7EmptyHost(t *testing.T) {
	var s osc7Scanner
	path, ok := s.feed([]byte("\x1b]7;file:///var/log\x07"))
	if !ok || path != "/var/log" {
		t.Fatalf("got %q ok=%v", path, ok)
	}
}
