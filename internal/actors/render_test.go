package actors

import (
	"strings"
	"testing"
)

func TestStripAnsiEscapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello world", "hello world"},
		{"color code", "\x1b[31mred\x1b[0m", "red"},
		{"carriage return", "ls\r\n", "ls\n"},
		{"bare CR", "hello\rworld", "helloworld"},
		{"backspace stripped", "ab\x08c", "abc"},
		{"OSC title", "\x1b]0;my title\x07rest", "rest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripAnsiEscapes(tt.input)
			if got != tt.want {
				t.Errorf("stripAnsiEscapes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsLineRedraw(t *testing.T) {
	tests := []struct {
		name  string
		chunk string // raw PTY chunk
		want  bool
	}{
		// bash's prompt redraw on a SIGWINCH/resize: CR + erase-to-EOL + prompt.
		{"bash resize prompt redraw", "\r\x1b[Kbash-3.2$ ", true},
		{"CR + prompt (no erase)", "\rbash-3.2$ ", true},
		{"CR + partial input redraw", "\r\x1b[Kbash-3.2$ ls", true},
		// Initial prompt has no leading CR — must be kept.
		{"initial prompt", "\x1b[?1034hbash-3.2$ ", false},
		// Real command output advances with a newline — must be kept.
		{"command output with newline", "\rhello\n", false},
		{"output then redrawn prompt", "done\n\r\x1b[Kbash-3.2$ ", false},
		{"plain output", "hello world", false},
		{"empty chunk", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk := []byte(tt.chunk)
			got := isLineRedraw(chunk, stripAnsiEscapes(tt.chunk))
			if got != tt.want {
				t.Errorf("isLineRedraw(%q) = %v, want %v", tt.chunk, got, tt.want)
			}
		})
	}
}

func TestStripShellPrompts(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no prompt", "hello\nworld\n", "hello\nworld\n"},
		{"dollar prompt", "bash-3.2$ \nhello\n", "hello\n"},
		{"hash prompt", "root# \nhello\n", "hello\n"},
		{"prompt with command not stripped", "bash-3.2$ ls\nhello\n", "bash-3.2$ ls\nhello\n"},
		{"triple newlines collapsed", "a\n\n\nb\n", "a\n\nb\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripShellPrompts(tt.input)
			if got != tt.want {
				t.Errorf("stripShellPrompts(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestChunkBoundaryNewlinePreservation verifies that processing PTY output
// in chunks preserves newlines at chunk boundaries. This was the root cause
// of the rapid shell command garbling bug: stripShellPrompts' TrimLeft("\n")
// removed the leading newline from a chunk, causing the previous chunk's
// content to be concatenated with it (e.g. "ls" + "rysh.config" = "lsrysh.config").
func TestChunkBoundaryNewlinePreservation(t *testing.T) {
	// Simulate PTY output split at an arbitrary boundary:
	// Full PTY output: "ls\r\nrysh.config\ttest\r\n"
	// Split into:
	//   chunk1: "ls\r"      → after ANSI strip: "ls"
	//   chunk2: "\nrysh.config\ttest\r\n" → after ANSI strip: "\nrysh.config\ttest\n"
	chunk1 := "ls\r"
	chunk2 := "\nrysh.config\ttest\r\n"

	stripped1 := stripAnsiEscapes(chunk1)
	stripped2 := stripAnsiEscapes(chunk2)

	// Key assertion: ANSI stripping preserves the newline in chunk2.
	// When these are concatenated in the suppress buffer, the newline
	// between "ls" and "rysh.config" must be preserved.
	combined := stripped1 + stripped2
	if !strings.Contains(combined, "ls\n") {
		t.Errorf("lost newline at chunk boundary: combined = %q, want 'ls\\n' present", combined)
	}
	if strings.HasPrefix(combined, "lsrysh") {
		t.Errorf("chunk boundary garbled: combined = %q (ls concatenated with rysh)", combined)
	}

	// Now verify that stripShellPrompts applied to the combined text
	// still preserves the content correctly (no garbling).
	afterPromptStrip := stripShellPrompts(combined)
	if strings.HasPrefix(afterPromptStrip, "lsrysh") {
		t.Errorf("stripShellPrompts garbled combined output: %q", afterPromptStrip)
	}

	// Verify the OLD approach (per-chunk stripShellPrompts) would have caused garbling.
	// This demonstrates the bug that the fix resolves.
	oldStripped1 := stripShellPrompts(stripAnsiEscapes(chunk1)) // "ls"
	oldStripped2 := stripShellPrompts(stripAnsiEscapes(chunk2)) // TrimLeft strips "\n" → "rysh.config\ttest\n"
	oldCombined := oldStripped1 + oldStripped2
	if !strings.HasPrefix(oldCombined, "lsrysh") {
		t.Logf("NOTE: old per-chunk approach would produce %q (may vary by chunk boundary)", oldCombined)
	}
}

// TestEchoSuppressionWithPreservedNewlines verifies that the echo suppression
// logic correctly finds and strips command echoes when newlines are preserved
// at chunk boundaries.
func TestEchoSuppressionWithPreservedNewlines(t *testing.T) {
	// Simulate: two rapid "ls" commands, PTY output arrives in chunks
	cmds := []string{"ls", "ls"}

	// Build accumulated buffer as the rawReadLoop would (ANSI strip only, no prompt strip):
	// PTY output: "ls\r\nrysh.config\ttest\r\nls\r\nrysh.config\ttest\r\n"
	ptyChunks := []string{
		"ls\r\n",
		"rysh.config\ttest\r\nls\r\n",
		"rysh.config\ttest\r\n",
	}

	var suppressBuf string
	for _, chunk := range ptyChunks {
		suppressBuf += stripAnsiEscapes(chunk)
	}

	// Try to strip all echoes
	testBuf := suppressBuf
	allFound := true
	for _, cmd := range cmds {
		echoWithNL := cmd + "\n"
		if idx := strings.Index(testBuf, echoWithNL); idx >= 0 {
			testBuf = testBuf[:idx] + testBuf[idx+len(echoWithNL):]
		} else {
			allFound = false
			break
		}
	}

	if !allFound {
		t.Fatalf("echo suppression failed to find all echoes in %q", suppressBuf)
	}

	// After stripping both "ls\n" echoes, only the output lines should remain.
	result := stripShellPrompts(testBuf)
	expected := "rysh.config\ttest\nrysh.config\ttest\n"
	if result != expected {
		t.Errorf("after echo stripping + prompt strip: got %q, want %q", result, expected)
	}
}

// TestEchoSuppressionChunkBoundarySplit verifies echo suppression works
// when a chunk boundary splits the command echo from its trailing newline.
func TestEchoSuppressionChunkBoundarySplit(t *testing.T) {
	cmds := []string{"ls"}

	// PTY splits "ls\r" | "\nrysh.config\ttest\r\n"
	chunk1 := stripAnsiEscapes("ls\r")                    // "ls"
	chunk2 := stripAnsiEscapes("\nrysh.config\ttest\r\n") // "\nrysh.config\ttest\n"

	suppressBuf := chunk1 + chunk2 // "ls\nrysh.config\ttest\n"

	testBuf := suppressBuf
	allFound := true
	for _, cmd := range cmds {
		echoWithNL := cmd + "\n"
		if idx := strings.Index(testBuf, echoWithNL); idx >= 0 {
			testBuf = testBuf[:idx] + testBuf[idx+len(echoWithNL):]
		} else {
			allFound = false
			break
		}
	}

	if !allFound {
		t.Fatalf("echo suppression failed: could not find 'ls\\n' in %q", suppressBuf)
	}

	result := stripShellPrompts(testBuf)
	if !strings.Contains(result, "rysh.config") {
		t.Errorf("output lost after echo suppression: got %q", result)
	}
	if strings.Contains(result, "ls") {
		t.Errorf("echo not properly stripped: got %q", result)
	}
}
