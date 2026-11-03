// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// TestAnsaKeysReachThePTY sends named keys to a pane of a test daemon through
// all three addressing forms and both doors, and checks the exact bytes the
// pane's program read.
//
// The program is `head -c 8` under `stty raw -echo`: raw mode passes every
// byte through unchanged (ESC, CR, ctrl+u), and head writes the file only once
// it has read exactly 8 bytes, so a stray byte (a refused Enter that leaked)
// shows up as a wrong file rather than a lucky pass.
func TestAnsaKeysReachThePTY(t *testing.T) {
	s := daemontest.Fresh(t)

	list := s.MustSucceed(t, "##pane list")
	m := listPaneID.FindStringSubmatch(list)
	if m == nil {
		t.Fatalf("no pane id in ##pane list:\n%s", list)
	}
	pane := m[1]
	const sid = "0f0f0f0f-1111-2222-3333-444444444444"
	s.MustSucceed(t, "##pane meta set claude.session_id "+sid)

	dump := filepath.Join(t.TempDir(), "keys.bin")
	startRawReader(t, s, "stty raw -echo; head -c 8 > "+dump+"; stty sane")

	// Every CLI call names the test session and an explicit sender, so nothing
	// inherited from the caller's environment (RYSH_SESSION, RYSH_PANE) can
	// point it at another session.
	keys := func(args ...string) (string, int) {
		return s.CLI(t, append([]string{"ansa", "keys", "--session", s.Name, "--as", "daemontest-caller"}, args...)...)
	}

	// 1. A guarded key without --force is refused and writes nothing.
	if out, code := keys(pane, "--", "enter"); code == 0 || !strings.Contains(out, "needs_force") {
		t.Fatalf("enter without --force: exit %d, want a needs_force refusal:\n%s", code, out)
	}
	// 2. By pane id: ESC, then up (ESC [ A).
	if out, code := keys(pane, "--", "esc", "up"); code != 0 || !strings.Contains(out, "keys sent to") {
		t.Fatalf("keys by pane id: exit %d\n%s", code, out)
	}
	// 3. By claude session id: ctrl+u.
	if out, code := keys("--claude-session", sid, "--", "ctrl+u"); code != 0 {
		t.Fatalf("keys by claude session: exit %d\n%s", code, out)
	}
	// 4. The ## door: down (ESC [ B).
	s.MustSucceed(t, "##ansa keys "+pane+" down")

	want := []byte("\x1b\x1b[A\x15\x1b[B")
	var got []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(dump); err == nil && len(b) == len(want) {
			got = b
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !bytes.Equal(got, want) {
		b, _ := os.ReadFile(dump)
		t.Fatalf("the pane read %q, want %q", b, want)
	}

	// The audit log: three sends and the one refusal, key names only.
	audit := findFile(t, s.Dir, "ansa-keys-audit.jsonl")
	text := string(audit)
	if n := strings.Count(text, `"status":"sent"`); n != 3 {
		t.Errorf("audit has %d sent lines, want 3:\n%s", n, text)
	}
	if n := strings.Count(text, `"code":"needs_force"`); n != 1 {
		t.Errorf("audit has %d needs_force lines, want 1:\n%s", n, text)
	}
	for _, w := range []string{`"keys":"esc up"`, `"keys":"ctrl+u"`, `"keys":"down"`, `"door":"##"`,
		`"ref":"claude-session:` + sid + `"`, `"from":"daemontest-caller"`, `"target":"` + pane + `"`} {
		if !strings.Contains(text, w) {
			t.Errorf("audit lacks %s:\n%s", w, text)
		}
	}
}

// TestAnsaKeysRefusals checks refusals exit non-zero against a live daemon.
func TestAnsaKeysRefusals(t *testing.T) {
	s := daemontest.Shared(t)
	keys := func(args ...string) (string, int) {
		return s.CLI(t, append([]string{"ansa", "keys", "--session", s.Name, "--as", "daemontest-caller"}, args...)...)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"no-such-pane", "--", "esc"}, "unknown_target"},
		{[]string{"--claude-session", "no-such-session", "--", "esc"}, "unknown_target"},
		{[]string{"no-such-pane", "--", "hello"}, "unknown key"},
		{[]string{"no-such-pane", "--", "1"}, "unknown key"},
		{[]string{"--", "esc"}, "needs a target"},
	} {
		out, code := keys(tc.args...)
		if code == 0 || !strings.Contains(out, tc.want) {
			t.Errorf("rysh ansa keys %q: exit %d, want non-zero with %q:\n%s", tc.args, code, tc.want, out)
		}
	}
	s.MustFail(t, "##ansa keys no-such-pane esc")
	s.MustFail(t, "##ansa keys --fleet x esc")
}

// startRawReader types cmd into the session's pane and waits until head is the
// foreground program. It re-sends only while nothing is running, so a slow
// shell never gets the command twice into head's input.
func startRawReader(t *testing.T, s *daemontest.Session, cmd string) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		list := s.MustSucceed(t, "##pane list")
		if strings.Contains(list, "running=head") {
			return
		}
		if attempt%25 == 0 && !strings.Contains(list, "running=") {
			s.MustSucceed(t, "##cmd ws "+cmd)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("head never became the pane's foreground program")
}

// findFile returns the contents of the first file named name under dir.
func findFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	var found string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no %s under the session directory", name)
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAnsaInterruptViaExecReachesThePTY guards the Esc path that works on a
// daemon without `ansa keys`: `rysh exec -- '##ansa interrupt <pane-id>'`
// writes exactly one ESC to the named pane's PTY, and nothing else.
func TestAnsaInterruptViaExecReachesThePTY(t *testing.T) {
	s := daemontest.Fresh(t)
	m := listPaneID.FindStringSubmatch(s.MustSucceed(t, "##pane list"))
	if m == nil {
		t.Fatal("no pane id in ##pane list")
	}
	dump := filepath.Join(t.TempDir(), "esc.bin")
	// head -c 2 with two interrupts: each must add exactly one ESC. An echoed
	// command or a CR would land in the file instead.
	startRawReader(t, s, "stty raw -echo; head -c 2 > "+dump+"; stty sane")
	s.MustSucceed(t, "##ansa interrupt "+m[1])
	s.MustSucceed(t, "##ansa interrupt "+m[1])

	var got []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(dump); err == nil && len(b) == 2 {
			got = b
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !bytes.Equal(got, []byte{0x1b, 0x1b}) {
		b, _ := os.ReadFile(dump)
		t.Fatalf("two ##ansa interrupt calls gave the pane %q, want two ESC bytes", b)
	}
}
