package actors

import "testing"

// hist is the shared history fixture (oldest first, newest last).
//
//	#1 echo first
//	#2 ls -la /tmp
//	#3 git commit -m wip
//	#4 make build test   <- previous command (!!)
var hist = []string{"echo first", "ls -la /tmp", "git commit -m wip", "make build test"}

func TestExpandHistory_Success(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"event !!", "!!", "make build test"},
		{"last arg !$", "echo !$", "echo test"},
		{"first arg !^", "echo !^", "echo build"},
		{"all args !*", "echo !*", "echo build test"},
		{"absolute !1", "!1", "echo first"},
		{"absolute !2", "!2", "ls -la /tmp"},
		{"absolute !4", "!4", "make build test"},
		{"relative !-1", "!-1", "make build test"},
		{"relative !-2", "!-2", "git commit -m wip"},
		{"prefix !ls", "!ls", "ls -la /tmp"},
		{"prefix !git", "!git", "git commit -m wip"},
		{"prefix !echo", "!echo", "echo first"},
		{"contains !?commit?", "!?commit?", "git commit -m wip"},
		{"contains no-close !?commit", "!?commit", "git commit -m wip"},
		{"contains !?-la?", "!?-la?", "ls -la /tmp"},
		{"two refs in one line", "cp !^ !$", "cp build test"},
		{"ref embedded after text", "sudo !!", "sudo make build test"},
		{"double quotes expand", `echo "!ls"`, `echo "ls -la /tmp"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := expandHistory(tt.in, hist)
			if err != nil {
				t.Fatalf("expandHistory(%q) unexpected error: %v", tt.in, err)
			}
			if !changed {
				t.Errorf("expandHistory(%q) changed=false, want true", tt.in)
			}
			if got != tt.want {
				t.Errorf("expandHistory(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpandHistory_QuickSubstitution(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"basic ^old^new", "^build^rebuild", "make rebuild test"},
		{"trailing caret", "^build^rebuild^", "make rebuild test"},
		{"trailing text after caret", "^build^rebuild^ now", "make rebuild test now"},
		{"delete with empty new", "^ test^", "make build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := expandHistory(tt.in, hist)
			if err != nil {
				t.Fatalf("expandHistory(%q) unexpected error: %v", tt.in, err)
			}
			if !changed {
				t.Errorf("expandHistory(%q) changed=false, want true", tt.in)
			}
			if got != tt.want {
				t.Errorf("expandHistory(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpandHistory_Literal(t *testing.T) {
	// These inputs contain '!' or '^' but must be returned unchanged.
	tests := []struct {
		name string
		in   string
	}{
		{"no special chars", "echo no bang here"},
		{"bang at end of line", "echo hello!"},
		{"bang then space", "find . ! -name x"},
		{"bang equals", "test a != b"},
		{"bang then close quote", `echo "done!"`},
		{"bang then paren", "arr=( !)"},
		{"single quotes suppress", "echo 'literal !! here'"},
		{"backslash escapes bang", `echo \!\!`},
		{"bare caret no text", "^"},
		{"caret empty old", "^^foo"},
		{"bang dash no digit", "echo !-x"},
		{"bang hash unsupported", "echo !#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := expandHistory(tt.in, hist)
			if err != nil {
				t.Fatalf("expandHistory(%q) unexpected error: %v", tt.in, err)
			}
			if changed {
				t.Errorf("expandHistory(%q) changed=true, want false (got %q)", tt.in, got)
			}
			if got != tt.in {
				t.Errorf("expandHistory(%q) = %q, want unchanged", tt.in, got)
			}
		})
	}
}

func TestExpandHistory_Errors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		h    []string
	}{
		{"!! empty history", "!!", nil},
		{"!$ empty history", "echo !$", nil},
		{"!n out of range", "!99", hist},
		{"!n zero", "!0", hist},
		{"!-n out of range", "!-99", hist},
		{"prefix not found", "!nonexistent", hist},
		{"contains not found", "!?zzzzz?", hist},
		{"quicksub no match", "^missing^x", hist},
		{"quicksub empty history", "^a^b", nil},
		{"first-arg bad specifier", "!^", []string{"solo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, changed, err := expandHistory(tt.in, tt.h)
			if err == nil {
				t.Errorf("expandHistory(%q) err=nil, want error", tt.in)
			}
			if changed {
				t.Errorf("expandHistory(%q) changed=true, want false on error", tt.in)
			}
		})
	}
}

// TestExpandHistory_NoArgsStar verifies !* expands to empty when the previous
// command has no arguments (matching bash).
func TestExpandHistory_NoArgsStar(t *testing.T) {
	got, changed, err := expandHistory("echo !*", []string{"ls"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("changed=false, want true")
	}
	if got != "echo " {
		t.Errorf("got %q, want %q", got, "echo ")
	}
}
