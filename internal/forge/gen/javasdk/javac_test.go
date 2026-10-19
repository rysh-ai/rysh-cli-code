// SPDX-License-Identifier: Apache-2.0

package javasdk

// Locating a javac that can actually compile the emitted SDK, and saying why
// when none exists.
//
// The emitted client targets Java `javaRelease` (javasdk.go). A javac older
// than that cannot compile it however correct the generator is, so
// TestJavaEmittedCompiles has to tell "the generator is broken" apart from
// "this machine's compiler is too old". It used to do neither: it checked only
// whether javac was PRESENT, so an old javac fell through to the compile and
// went red — the last unfixed item of F-17, and the only red test in the
// rysh-cli suite.
//
// Everything here is test-only: the generator emits Java source as string data
// and never shells out to a compiler.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
)

// leadingInt reads the run of digits at the start of s. Version strings carry
// suffixes ("21-ea", "17.0.9+11") that Atoi rejects outright.
func leadingInt(s string) (int, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("no leading integer in %q", s)
	}
	return strconv.Atoi(s[:i])
}

// parseJavacVersion extracts the Java feature version from `javac -version`
// output. It understands the legacy "javac 1.8.0_302" form, where the feature
// number is the SECOND component (1.8 means 8, not 1), and the modern
// "javac 17.0.9" form, where it is the first.
//
// Callers must pass COMBINED output: javac prints its version to stderr on
// JDK 8 and to stdout on modern JDKs, so reading stdout alone yields "" on
// exactly the old compilers this gate exists to catch.
func parseJavacVersion(out string) (int, error) {
	fields := strings.Fields(out)
	var ver string
	switch {
	case len(fields) >= 2 && strings.EqualFold(fields[0], "javac"):
		ver = fields[1]
	case len(fields) == 1:
		ver = fields[0] // tolerate a bare version with no "javac" prefix
	default:
		return 0, fmt.Errorf("unrecognised javac version output %q", out)
	}
	parts := strings.Split(ver, ".")
	first, err := leadingInt(parts[0])
	if err != nil {
		return 0, fmt.Errorf("unrecognised javac version %q: %w", ver, err)
	}
	if first != 1 {
		return first, nil
	}
	// Legacy "1.N.x" naming: the feature version is N.
	if len(parts) < 2 {
		return 0, fmt.Errorf("truncated legacy javac version %q", ver)
	}
	second, err := leadingInt(parts[1])
	if err != nil {
		return 0, fmt.Errorf("unrecognised legacy javac version %q: %w", ver, err)
	}
	return second, nil
}

// javacUsable reports whether a javac at feature version `found` can compile
// sources declaring release `required`, and when it cannot, the reason to cite
// in the skip.
//
// A usable compiler yields an EMPTY reason, so a caller cannot skip without
// having one to print. That is the property that keeps a version gate from
// degrading into "skip always", which would read as green while testing
// nothing.
func javacUsable(found, required int) (bool, string) {
	if found >= required {
		return true, ""
	}
	return false, fmt.Sprintf(
		"javac is Java %d, but the emitted SDK declares "+
			"<maven.compiler.release>%d</maven.compiler.release> (javaRelease in javasdk.go): "+
			"a Java %d compiler has neither java.net.http nor URLEncoder.encode(String, Charset), "+
			"so this compile would fail on the toolchain, not on the generated code. "+
			"Install a JDK >= %d, or point JAVA_HOME at one, to run this compile for real.",
		found, required, found, required)
}

// javacVersionOf runs a javac and reports the feature version it claims.
func javacVersionOf(path string) (int, error) {
	out, err := exec.Command(path, "-version").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%s -version: %w", path, err)
	}
	return parseJavacVersion(string(out))
}

// findJavac returns the newest javac it can find, and the feature version that
// javac reported. `required` only shapes where it looks; the caller applies
// javacUsable to what comes back. An error means no javac at all.
//
// Every candidate is verified by EXECUTING it. None is trusted on the strength
// of where it came from — which matters concretely here: on this machine
// (macOS 26.5, one JDK installed) `/usr/libexec/java_home -v 11+` exits 0 and
// prints the Temurin *8* home, as does `-v 99`. Believing that lookup would
// hand the test a Java 8 compiler and leave it red, which is the exact bug
// this gate removes.
func findJavac(required int) (string, int, error) {
	var candidates []string
	if home := strings.TrimSpace(os.Getenv("JAVA_HOME")); home != "" {
		candidates = append(candidates, filepath.Join(home, "bin", "javac"))
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/usr/libexec/java_home", "-v", fmt.Sprintf("%d+", required)).Output()
		if err == nil {
			if home := strings.TrimSpace(string(out)); home != "" {
				candidates = append(candidates, filepath.Join(home, "bin", "javac"))
			}
		}
	}
	if p, err := exec.LookPath("javac"); err == nil {
		candidates = append(candidates, p)
	}

	seen := make(map[string]bool, len(candidates))
	bestPath, bestVer := "", 0
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		v, err := javacVersionOf(c)
		if err != nil {
			continue
		}
		if v > bestVer {
			bestPath, bestVer = c, v
		}
	}
	if bestPath == "" {
		return "", 0, fmt.Errorf("no working javac found (looked at %d candidate(s))", len(seen))
	}
	return bestPath, bestVer, nil
}

func TestParseJavacVersion(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
	}{
		{"legacy 8", "javac 1.8.0_302", 8},
		{"legacy 8 on stderr with newline", "javac 1.8.0_302\n", 8},
		{"legacy 7", "javac 1.7.0_80", 7},
		{"modern 11", "javac 11.0.2", 11},
		{"modern 17 bare", "javac 17", 17},
		{"modern 21", "javac 21.0.1", 21},
		{"early access suffix", "javac 24-ea", 24},
		{"build suffix", "javac 17.0.9+11", 17},
		{"no javac prefix", "1.8.0_302", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJavacVersion(tc.out)
			if err != nil {
				t.Fatalf("parseJavacVersion(%q) returned error: %v", tc.out, err)
			}
			if got != tc.want {
				t.Fatalf("parseJavacVersion(%q) = %d, want %d", tc.out, got, tc.want)
			}
		})
	}

	bad := []struct {
		name string
		out  string
	}{
		{"empty", ""},
		{"whitespace", "   \n"},
		{"garbage word", "banana"},
		{"javac then garbage", "javac banana"},
		{"legacy truncated", "javac 1"},
		{"legacy non-numeric minor", "javac 1.x.0"},
		{"too many fields", "some unexpected preamble line"},
	}
	for _, tc := range bad {
		t.Run("error/"+tc.name, func(t *testing.T) {
			if got, err := parseJavacVersion(tc.out); err == nil {
				t.Fatalf("parseJavacVersion(%q) = %d, want an error", tc.out, got)
			}
		})
	}
}

// TestJavacUsable pins the gate's decision in both directions. The second half
// is the one that matters: a version gate that skips on every input looks
// green while compiling nothing, which is strictly worse than the red test it
// replaced.
func TestJavacUsable(t *testing.T) {
	t.Run("too old skips with a reason naming both versions", func(t *testing.T) {
		for _, found := range []int{7, 8, 10} {
			ok, reason := javacUsable(found, 11)
			if ok {
				t.Fatalf("javacUsable(%d, 11) = true, want false", found)
			}
			if reason == "" {
				t.Fatalf("javacUsable(%d, 11) gave no reason to cite", found)
			}
			if !strings.Contains(reason, fmt.Sprintf("Java %d", found)) {
				t.Errorf("reason does not name the version found (%d): %q", found, reason)
			}
			if !strings.Contains(reason, "maven.compiler.release>11<") {
				t.Errorf("reason does not name the required release or its source: %q", reason)
			}
		}
	})

	t.Run("sufficient version runs the compile", func(t *testing.T) {
		for _, found := range []int{11, 17, 21, 25} {
			ok, reason := javacUsable(found, 11)
			if !ok {
				t.Fatalf("javacUsable(%d, 11) = false, want true: the gate would skip a javac that can compile", found)
			}
			if reason != "" {
				t.Fatalf("javacUsable(%d, 11) returned a skip reason %q while reporting usable", found, reason)
			}
		}
	})

	t.Run("exactly at the required release runs", func(t *testing.T) {
		if ok, _ := javacUsable(javaRelease, javaRelease); !ok {
			t.Fatalf("javacUsable(%d, %d) = false: the declared release must be sufficient for itself",
				javaRelease, javaRelease)
		}
	})
}

// TestPomDeclaresJavaRelease keeps the emitted pom and the test gate on one
// number. If the pom stopped being generated from javaRelease, the gate could
// silently admit a compiler too old for the code actually emitted.
func TestPomDeclaresJavaRelease(t *testing.T) {
	fs, err := (&generator{}).Generate(sampleAPI(), gen.Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	pom, ok := fs.Files["pom.xml"]
	if !ok {
		t.Fatalf("no pom.xml in emitted file set (have %d files)", len(fs.Files))
	}
	want := fmt.Sprintf("<maven.compiler.release>%d</maven.compiler.release>", javaRelease)
	if !strings.Contains(string(pom), want) {
		t.Fatalf("emitted pom.xml does not declare %q:\n%s", want, pom)
	}
}
