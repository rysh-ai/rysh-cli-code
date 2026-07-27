package progname

import (
	"os"
	"testing"
)

// The substitution must catch the command word and nothing else: a filename or
// path that merely starts with "rysh" has to survive verbatim, or the `ry`
// build would tell users about a "ry.config.yaml" that does not exist.
func TestRewrite(t *testing.T) {
	orig := os.Args[0]
	os.Args[0] = "/usr/local/bin/ry"
	t.Cleanup(func() { os.Args[0] = orig })

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"bare command", "       rysh doctor", "       ry doctor"},
		{"usage header", "usage: rysh [--config <path>]", "usage: ry [--config <path>]"},
		{"quoted command", `"rysh list-sessions" flags`, `"ry list-sessions" flags`},
		{"prose", "Use after rebuilding rysh so the new version takes", "Use after rebuilding ry so the new version takes"},
		{"end of line", "then run rysh", "then run ry"},
		{"config filename", "writes rysh.config.yaml in the cwd", "writes rysh.config.yaml in the cwd"},
		{"dot-prefixed dir", "./rysh.config.yaml then ./.rysh/rysh.config.yaml.", "./rysh.config.yaml then ./.rysh/rysh.config.yaml."},
		{"log path", "logs are saved to /tmp/rysh/logs/", "logs are saved to /tmp/rysh/logs/"},
		{"lock file", "installed packages (.rysh/rysh.lock)", "installed packages (.rysh/rysh.lock)"},
		{"hyphenated", "see rysh-cli for details", "see rysh-cli for details"},
		{"underscored", "the rysh_local dev build", "the rysh_local dev build"},
		{"longer word", "ryshification is not a word", "ryshification is not a word"},
		{"twice on a line", "rysh attach; then rysh detach", "ry attach; then ry detach"},
		{"empty", "", ""},
		{"message prefix", "[rysh] no matching tab", "[rysh] no matching tab"},
		{"system command", "  ##rysh web stop            stop the web server", "  ##rysh web stop            stop the web server"},
		{"prefix then command", "[rysh] run rysh doctor", "[rysh] run ry doctor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Rewrite(tc.in); got != tc.want {
				t.Errorf("Rewrite(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Under the open-source name the text is already correct and must be returned
// untouched — including the filenames, which the fast path must not mangle.
func TestRewriteIsIdentityForRysh(t *testing.T) {
	orig := os.Args[0]
	os.Args[0] = "/usr/local/bin/rysh"
	t.Cleanup(func() { os.Args[0] = orig })

	in := "       rysh doctor writes rysh.config.yaml"
	if got := Rewrite(in); got != in {
		t.Errorf("Rewrite(%q) = %q, want it unchanged", in, got)
	}
}

func TestNameFallbacks(t *testing.T) {
	orig := os.Args[0]
	t.Cleanup(func() { os.Args[0] = orig })

	for _, tc := range []struct{ argv0, want string }{
		{"/usr/local/bin/ry", "ry"},
		{"/usr/local/bin/rysh", "rysh"},
		// Windows-style paths are not exercised here: filepath.Base only splits
		// on '\' when built for Windows, so the result would be host-dependent.
		{"ry.exe", "ry"},
		{"", "rysh"},
		{".", "rysh"},
		// `go test` builds a binary named <pkg>.test; usage output should not
		// depend on that.
		{"/tmp/go-build123/rysh.test", "rysh"},
	} {
		os.Args[0] = tc.argv0
		if got := Name(); got != tc.want {
			t.Errorf("Name() with argv[0]=%q = %q, want %q", tc.argv0, got, tc.want)
		}
	}
}
