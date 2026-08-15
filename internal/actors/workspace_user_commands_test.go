// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// writeCommand drops a script into <base>/commands/<filename>.
func writeCommand(t *testing.T, base, filename, body string) string {
	t.Helper()
	dir := filepath.Join(base, userCommandsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeSessionCommand drops a script into <base>/commands/<session>/<filename>.
func writeSessionCommand(t *testing.T, base, session, filename, body string) string {
	t.Helper()
	dir := filepath.Join(base, userCommandsSubdir, session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// collector is a sink that records everything a run published, in order.
type collector struct {
	mu   sync.Mutex
	text strings.Builder
}

func (c *collector) sink(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text.WriteString(s)
}

func (c *collector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

func TestValidUserCommandName(t *testing.T) {
	ok := []string{"nfre23", "deploy", "a", "my-cmd", "my_cmd", "v1.2", "Z9"}
	for _, name := range ok {
		if !validUserCommandName(name) {
			t.Errorf("validUserCommandName(%q) = false, want true", name)
		}
	}
	// Everything here either escapes the commands directory or cannot be typed
	// after "#" in the first place.
	bad := []string{"", ".hidden", "..", "../etc/passwd", "a/b", "a\\b", "-flag", "a b", "a;b", "a$b"}
	for _, name := range bad {
		if validUserCommandName(name) {
			t.Errorf("validUserCommandName(%q) = true, want false", name)
		}
	}
}

func TestFindUserCommandInResolvesBareNameAndSuffixes(t *testing.T) {
	base := t.TempDir()
	bare := writeCommand(t, base, "nfre23", "echo hi\n")
	suffixed := writeCommand(t, base, "deploy.sh", "echo deploy\n")
	writeCommand(t, base, "build.bash", "echo build\n")

	for _, tc := range []struct{ name, want string }{
		{"nfre23", bare},
		{"deploy", suffixed},
	} {
		got, ok := findUserCommandIn([]string{base}, "", tc.name)
		if !ok || got != tc.want {
			t.Errorf("findUserCommandIn(%q) = %q, %v; want %q, true", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := findUserCommandIn([]string{base}, "", "build"); !ok {
		t.Error("findUserCommandIn(build) did not resolve build.bash")
	}
	if _, ok := findUserCommandIn([]string{base}, "", "missing"); ok {
		t.Error("findUserCommandIn(missing) resolved something")
	}
}

// A directory is not a command. Without the IsRegular check, `bash <dir>` would
// be executed and fail with a message that names bash rather than rysh.
func TestFindUserCommandInIgnoresDirectories(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, userCommandsSubdir, "notacmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if path, ok := findUserCommandIn([]string{base}, "", "notacmd"); ok {
		t.Errorf("directory resolved as a command: %q", path)
	}
}

// The name is joined onto a directory, so a word that can climb out of it must
// be refused before it reaches the filesystem — not merely fail to be found.
func TestFindUserCommandInRefusesEscapes(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside.sh")
	if err := os.WriteFile(outside, []byte("echo pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside", "../outside.sh", "/etc/passwd", "sub/cmd"} {
		if path, ok := findUserCommandIn([]string{base}, "", name); ok {
			t.Errorf("findUserCommandIn(%q) resolved %q, want refusal", name, path)
		}
	}
}

// An earlier base shadows a later one, so a project-local command wins over the
// user's global one of the same name — the order ryshBaseDirs already uses for
// skills, secrets and variables.
func TestFindUserCommandInPrefersEarlierBase(t *testing.T) {
	local, home := t.TempDir(), t.TempDir()
	want := writeCommand(t, local, "deploy", "echo local\n")
	writeCommand(t, home, "deploy", "echo global\n")

	got, ok := findUserCommandIn([]string{local, home}, "", "deploy")
	if !ok || got != want {
		t.Errorf("findUserCommandIn = %q, %v; want %q, true", got, ok, want)
	}
}

// The session's own directory is searched before the shared one, which is what
// lets two sessions in the same checkout each define #deploy.
func TestFindUserCommandInPrefersTheSessionDir(t *testing.T) {
	base := t.TempDir()
	want := writeSessionCommand(t, base, "macmini-rysh", "deploy", "echo mine\n")
	writeCommand(t, base, "deploy", "echo shared\n")

	got, ok := findUserCommandIn([]string{base}, "macmini-rysh", "deploy")
	if !ok || got != want {
		t.Errorf("findUserCommandIn = %q, %v; want %q, true", got, ok, want)
	}
}

// A command with no copy in the session's directory still resolves from the
// shared one. Without this every command written before scoping — and every
// command a project ships for all its sessions — would silently stop answering.
func TestFindUserCommandInFallsBackToShared(t *testing.T) {
	base := t.TempDir()
	want := writeCommand(t, base, "deploy", "echo shared\n")

	got, ok := findUserCommandIn([]string{base}, "macmini-rysh", "deploy")
	if !ok || got != want {
		t.Errorf("findUserCommandIn = %q, %v; want %q, true", got, ok, want)
	}
}

// The point of the whole change: one session cannot run another's command.
func TestFindUserCommandInIgnoresAnotherSessionsDir(t *testing.T) {
	base := t.TempDir()
	writeSessionCommand(t, base, "other-session", "deploy", "echo theirs\n")

	if path, ok := findUserCommandIn([]string{base}, "macmini-rysh", "deploy"); ok {
		t.Errorf("resolved another session's command: %q", path)
	}
	// And the directory itself is not a command named after the session, even
	// though it sits directly under the shared directory that IS searched.
	if path, ok := findUserCommandIn([]string{base}, "macmini-rysh", "other-session"); ok {
		t.Errorf("a session directory resolved as a command: %q", path)
	}
}

// A session name is joined onto the commands directory, so one that is not a
// single safe segment must not be able to point the lookup elsewhere. It falls
// back to the shared directory rather than failing: the session still works, it
// simply has no scope of its own.
func TestUserCommandDirsRefusesAnUnusableSessionName(t *testing.T) {
	shared := filepath.Join("base", userCommandsSubdir)
	for _, session := range []string{"", "..", "../../etc", "a/b", ".hidden"} {
		got := userCommandDirs([]string{"base"}, session)
		if len(got) != 1 || got[0] != shared {
			t.Errorf("userCommandDirs(%q) = %v, want just %q", session, got, shared)
		}
	}
	if got := userCommandDirs([]string{"base"}, "macmini-rysh"); len(got) != 2 ||
		got[0] != filepath.Join(shared, "macmini-rysh") || got[1] != shared {
		t.Errorf("userCommandDirs(macmini-rysh) = %v", got)
	}
}

func TestListUserCommandsIn(t *testing.T) {
	local, home := t.TempDir(), t.TempDir()
	writeCommand(t, local, "beta", "echo b\n")
	writeCommand(t, local, "alpha.sh", "echo a\n")
	localDeploy := writeCommand(t, local, "deploy", "echo local\n")
	writeCommand(t, home, "deploy", "echo global\n")
	writeCommand(t, home, "gamma", "echo g\n")
	// Not invokable: nothing after "#" can produce this name.
	writeCommand(t, local, ".hidden", "echo h\n")

	got := listUserCommandsIn([]string{local, home}, "")

	var names []string
	for _, c := range got {
		names = append(names, c.name)
	}
	want := []string{"alpha", "beta", "deploy", "gamma"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, c := range got {
		if c.name == "deploy" && c.path != localDeploy {
			t.Errorf("deploy resolved to %q, want the shadowing local copy %q", c.path, localDeploy)
		}
	}
}

// A listing is what one session can actually run: its own commands, the shared
// ones, and nothing belonging to another session.
func TestListUserCommandsInIsScopedToTheSession(t *testing.T) {
	base := t.TempDir()
	mine := writeSessionCommand(t, base, "macmini-rysh", "deploy", "echo mine\n")
	writeSessionCommand(t, base, "macmini-rysh", "onlymine.sh", "echo mine\n")
	writeCommand(t, base, "deploy", "echo shared\n")
	writeCommand(t, base, "onlyshared", "echo shared\n")
	writeSessionCommand(t, base, "other-session", "theirs", "echo theirs\n")

	var names []string
	got := listUserCommandsIn([]string{base}, "macmini-rysh")
	for _, c := range got {
		names = append(names, c.name)
	}
	// "other-session" must not appear either: it is a directory under the shared
	// dir, not a command.
	want := []string{"deploy", "onlymine", "onlyshared"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, c := range got {
		if c.name == "deploy" && c.path != mine {
			t.Errorf("deploy resolved to %q, want this session's copy %q", c.path, mine)
		}
	}
}

func TestListUserCommandsInMissingDir(t *testing.T) {
	if got := listUserCommandsIn([]string{t.TempDir()}, ""); len(got) != 0 {
		t.Errorf("listUserCommandsIn(empty base) = %v, want none", got)
	}
}

func TestRunUserCommandPublishesOutputAndArgs(t *testing.T) {
	base := t.TempDir()
	path := writeCommand(t, base, "nfre23", "echo \"args: $*\"\n")

	var c collector
	kept, err := runUserCommand("nfre23", path, []string{"one", "two"}, os.Environ(), base, c.sink)
	if err != nil {
		t.Fatalf("runUserCommand: %v", err)
	}

	if got := c.String(); got != "args: one two\n" {
		t.Errorf("published %q, want %q", got, "args: one two\n")
	}
	// The returned text is what a `rysh exec` caller receives; it must match
	// what the pane saw, or the two audiences disagree about what happened.
	if kept != c.String() {
		t.Errorf("returned %q but published %q", kept, c.String())
	}
}

// A script needs no execute bit and no shebang: the contract is "bash runs it".
func TestRunUserCommandNeedsNoExecuteBit(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, userCommandsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plain")
	if err := os.WriteFile(path, []byte("echo ran\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var c collector
	runUserCommand("plain", path, nil, os.Environ(), base, c.sink)

	if got := c.String(); got != "ran\n" {
		t.Errorf("output = %q, want %q", got, "ran\n")
	}
}

// A failure has two audiences and only one of them is guaranteed to exist: a
// `rysh exec` caller reads the error, someone who typed #boom into a pane
// never sees it. Both have to be told.
func TestRunUserCommandReportsNonZeroExit(t *testing.T) {
	base := t.TempDir()
	path := writeCommand(t, base, "boom", "echo before\nexit 3\n")

	var c collector
	kept, err := runUserCommand("boom", path, nil, os.Environ(), base, c.sink)

	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("err = %v, want an exit-3 failure", err)
	}
	got := c.String()
	if !strings.Contains(got, "before\n") {
		t.Errorf("output %q lost the script's own stdout", got)
	}
	if !strings.Contains(got, "[rysh] #boom exited 3") {
		t.Errorf("output %q does not report the exit code", got)
	}
	if kept != got {
		t.Errorf("returned %q but published %q", kept, got)
	}
}

// A script that cannot even start is a failure too — bash reports it, and the
// caller must not read that as success.
func TestRunUserCommandReportsAStartFailure(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, userCommandsSubdir, "gone")

	var c collector
	_, err := runUserCommand("gone", missing, nil, os.Environ(), base, c.sink)
	if err == nil {
		t.Fatal("running a missing file reported success")
	}
}

func TestRunUserCommandMergesStderr(t *testing.T) {
	base := t.TempDir()
	path := writeCommand(t, base, "noisy", "echo out\necho err >&2\n")

	var c collector
	runUserCommand("noisy", path, nil, os.Environ(), base, c.sink)

	got := c.String()
	if !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Errorf("output = %q, want both streams", got)
	}
}

// The pane's address and the rysh binary are what let a user command drive the
// session it was typed into rather than merely run beside it.
func TestRunUserCommandExportsIdentity(t *testing.T) {
	base := t.TempDir()
	path := writeCommand(t, base, "ident",
		"echo \"$RYSH_COMMAND|$RYSH_PANE|$RYSH_TAB|$RYSH_SESSION|$RYSH_BIN|$RYSH_COMMAND_FILE\"\n")

	coords := paneCoords{TabID: "tab-1", LaneID: "lane-1", GroupID: "pg-1", PaneID: "pane-1"}
	env := userCommandEnv(os.Environ(), "/usr/local/bin/rysh", "sess", "ident", path, coords)

	var c collector
	runUserCommand("ident", path, nil, env, base, c.sink)

	want := "ident|pane-1|tab-1|sess|/usr/local/bin/rysh|" + path + "\n"
	if got := c.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// An outer session's exported identity must not survive into a command that
// belongs to this one — the same reason pane shells append their identity last.
func TestUserCommandEnvOverridesInheritedIdentity(t *testing.T) {
	base := []string{"RYSH_PANE=stale-pane", "RYSH_SESSION=stale-session"}
	env := userCommandEnv(base, "", "fresh", "n", "/p", paneCoords{PaneID: "pane-9"})

	last := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			last[k] = v
		}
	}
	if last["RYSH_PANE"] != "pane-9" {
		t.Errorf("RYSH_PANE = %q, want pane-9", last["RYSH_PANE"])
	}
	if last["RYSH_SESSION"] != "fresh" {
		t.Errorf("RYSH_SESSION = %q, want fresh", last["RYSH_SESSION"])
	}
}

// stdin is closed rather than inherited: the pane's terminal belongs to the
// pane's own shell, and a script reading it would block behind a prompt nobody
// can answer.
func TestRunUserCommandHasNoStdin(t *testing.T) {
	base := t.TempDir()
	path := writeCommand(t, base, "reader", "read -r line && echo \"got:$line\" || echo eof\n")

	done := make(chan string, 1)
	go func() {
		var c collector
		runUserCommand("reader", path, nil, os.Environ(), base, c.sink)
		done <- c.String()
	}()

	select {
	case got := <-done:
		if !strings.Contains(got, "eof") {
			t.Errorf("output = %q, want the read to hit EOF", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("script blocked on stdin")
	}
}

func TestUserCommandOutTruncates(t *testing.T) {
	var c collector
	out := &userCommandOut{sink: c.sink, limit: 10}

	n, err := out.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write = %d, %v; want 16, nil", n, err)
	}
	// Draining continues after the cap so the script sees no EPIPE and is not
	// killed for being chatty.
	if n, err := out.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("Write after cap = %d, %v; want 4, nil", n, err)
	}
	if !out.truncated {
		t.Error("truncated flag not set")
	}
	if got := c.String(); got != "0123456789" {
		t.Errorf("published %q, want the first 10 bytes only", got)
	}
}

func TestUserCommandOutFinishAddsMissingNewline(t *testing.T) {
	var c collector
	out := &userCommandOut{sink: c.sink, limit: 100}
	_, _ = out.Write([]byte("no trailing newline"))
	out.finish()
	if got := c.String(); !strings.HasSuffix(got, "\n") {
		t.Errorf("output %q does not end in a newline", got)
	}

	var c2 collector
	out2 := &userCommandOut{sink: c2.sink, limit: 100}
	_, _ = out2.Write([]byte("has one\n"))
	out2.finish()
	if got := c2.String(); got != "has one\n" {
		t.Errorf("output = %q, want no extra newline", got)
	}
}

// Nothing published for a command that produced no output at all: a silent
// script should look exactly like a silent shell command.
func TestUserCommandOutStaysSilentWhenEmpty(t *testing.T) {
	var c collector
	out := &userCommandOut{sink: c.sink, limit: 100}
	out.finish()
	if got := c.String(); got != "" {
		t.Errorf("published %q for a silent command", got)
	}
}

func TestUserCommandNameFor(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"nfre23", "nfre23"},
		{"deploy.sh", "deploy"},
		{"build.bash", "build"},
		{"notes.md", "notes.md"},
		{".hidden", ""},
		{"", ""},
	} {
		if got := userCommandNameFor(tc.file); got != tc.want {
			t.Errorf("userCommandNameFor(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// The unknown-command message is the only place a user who has not read the
// docs will learn that ##nfre23 could be made to work.
func TestRyshUnknownCommandPointsAtTheCommandsDir(t *testing.T) {
	t.Chdir(t.TempDir())
	var w WorkspaceActor
	var out strings.Builder
	w.ryshUnknownCommand(&out, "nfre23")

	got := out.String()
	if !strings.Contains(got, filepath.Join(userCommandsSubdir, "nfre23")) {
		t.Errorf("message %q does not name the file that would define it", got)
	}
	if !strings.Contains(got, "##commands new nfre23") {
		t.Errorf("message %q does not point at ##commands new", got)
	}
	// The 180-line help is what this replaces; printing both would defeat it.
	if strings.Contains(got, "available ## commands:") {
		t.Errorf("message %q fell through to the full help", got)
	}
}

// A word that could never name a file still gets the old behaviour.
func TestRyshUnknownCommandFallsBackToHelp(t *testing.T) {
	t.Chdir(t.TempDir())
	var w WorkspaceActor
	var out strings.Builder
	w.ryshUnknownCommand(&out, "$$$")

	if !strings.Contains(out.String(), "available ## commands:") {
		t.Errorf("message %q did not fall back to the help", out.String())
	}
}

func TestUserCommandsCommand(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	var w WorkspaceActor

	// new
	var out strings.Builder
	if err := w.handleUserCommandsCommand(&out, []string{"new", "nfre23"}); err != nil {
		t.Fatalf("##commands new: %v", err)
	}
	path := filepath.Join(".rysh", userCommandsSubdir, "nfre23")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("##commands new did not create %s: %v", path, err)
	}

	// a second new refuses rather than overwriting the user's script
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"new", "nfre23"}); err == nil {
		t.Error("##commands new on an existing command did not fail")
	}

	// A built-in name is fine now: #pane and ##pane are different words. The
	// earlier cut had to refuse this, back when both lived under ##.
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"new", "pane"}); err != nil {
		t.Errorf("##commands new pane was refused: %v", err)
	}

	// list
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"list"}); err != nil {
		t.Fatalf("##commands list: %v", err)
	}
	if !strings.Contains(out.String(), "#nfre23") {
		t.Errorf("##commands list = %q, missing the new command", out.String())
	}

	// show
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"show", "nfre23"}); err != nil {
		t.Fatalf("##commands show: %v", err)
	}
	if !strings.Contains(out.String(), "hello from #nfre23") {
		t.Errorf("##commands show = %q, missing the script body", out.String())
	}

	// show of something that does not exist is a failure, not empty output
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"show", "nope"}); err == nil {
		t.Error("##commands show nope did not fail")
	}

	// path
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"path"}); err != nil {
		t.Fatalf("##commands path: %v", err)
	}
	if !strings.Contains(out.String(), userCommandsSubdir) {
		t.Errorf("##commands path = %q", out.String())
	}

	// unknown subcommand
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"frobnicate"}); err == nil {
		t.Error("##commands frobnicate did not fail")
	}
}

// `##commands new` writes into the calling session's directory, so the default
// for a new command is "mine" rather than "everyone's".
func TestUserCommandsNewWritesTheSessionDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	w := WorkspaceActor{sessionName: "macmini-rysh"}

	var out strings.Builder
	if err := w.handleUserCommandsCommand(&out, []string{"new", "nfre23"}); err != nil {
		t.Fatalf("##commands new: %v", err)
	}
	scoped := filepath.Join(".rysh", userCommandsSubdir, "macmini-rysh", "nfre23")
	if _, err := os.Stat(scoped); err != nil {
		t.Fatalf("##commands new did not create %s: %v", scoped, err)
	}
	if _, err := os.Stat(filepath.Join(".rysh", userCommandsSubdir, "nfre23")); err == nil {
		t.Error("##commands new also wrote the shared directory")
	}

	// --shared is the way to write one every session sees.
	out.Reset()
	if err := w.handleUserCommandsCommand(&out, []string{"new", "everywhere", "--shared"}); err != nil {
		t.Fatalf("##commands new --shared: %v", err)
	}
	shared := filepath.Join(".rysh", userCommandsSubdir, "everywhere")
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("##commands new --shared did not create %s: %v", shared, err)
	}

	// Both resolve for this session; only the shared one resolves for another.
	if got, ok := w.findUserCommand("nfre23"); !ok || got != scoped {
		t.Errorf("findUserCommand(nfre23) = %q, %v", got, ok)
	}
	other := WorkspaceActor{sessionName: "other-session"}
	if _, ok := other.findUserCommand("nfre23"); ok {
		t.Error("another session resolved this session's command")
	}
	if _, ok := other.findUserCommand("everywhere"); !ok {
		t.Error("another session could not resolve the shared command")
	}
}

// A shared command already answering in this session is not silently shadowed:
// `new` refuses and names the file it found, because a script that quietly
// takes over a working word is the surprise scoping could most easily add.
func TestUserCommandsNewRefusesToShadowASharedCommand(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	shared := writeCommand(t, ".rysh", "deploy", "echo shared\n")

	w := WorkspaceActor{sessionName: "macmini-rysh"}
	var out strings.Builder
	if err := w.handleUserCommandsCommand(&out, []string{"new", "deploy"}); err == nil {
		t.Fatal("##commands new over a shared command did not fail")
	}
	if !strings.Contains(out.String(), shared) {
		t.Errorf("##commands new = %q, does not name the file it found", out.String())
	}
}

func TestExtractUserCommandShared(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		want   []string
		shared bool
	}{
		{[]string{"nfre23"}, []string{"nfre23"}, false},
		{[]string{"nfre23", "--shared"}, []string{"nfre23"}, true},
		{[]string{"--shared", "nfre23"}, []string{"nfre23"}, true},
		{nil, []string{}, false},
	} {
		got, shared := extractUserCommandShared(tc.args)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") || shared != tc.shared {
			t.Errorf("extractUserCommandShared(%v) = %v, %v; want %v, %v", tc.args, got, shared, tc.want, tc.shared)
		}
	}
}

// `##commands path` has to name the session's directory, since that is where
// the next `new` lands and the only place a scoped command can be dropped by
// hand.
func TestUserCommandsPathNamesTheSessionDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	w := WorkspaceActor{sessionName: "macmini-rysh"}

	var out strings.Builder
	if err := w.handleUserCommandsCommand(&out, []string{"path"}); err != nil {
		t.Fatalf("##commands path: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, filepath.Join(userCommandsSubdir, "macmini-rysh")) {
		t.Errorf("##commands path = %q, does not name the session directory", got)
	}
	if !strings.Contains(got, "--shared") {
		t.Errorf("##commands path = %q, does not say how to write a shared one", got)
	}
}

// The generated template must be a script that actually runs — a starter file
// with a syntax error would be a bad first experience.
func TestUserCommandsNewTemplateRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	var w WorkspaceActor
	var out strings.Builder
	if err := w.handleUserCommandsCommand(&out, []string{"new", "greet"}); err != nil {
		t.Fatalf("##commands new: %v", err)
	}

	path, ok := w.findUserCommand("greet")
	if !ok {
		t.Fatal("the command just created does not resolve")
	}
	var c collector
	runUserCommand("greet", path, []string{"world"}, os.Environ(), dir, c.sink)
	if got := c.String(); got != "hello from #greet world\n" {
		t.Errorf("template output = %q", got)
	}
}

// The CLI reply is deferred until the script finishes, which is the only
// reason `rysh exec -- "nfre23"` sees anything at all: handleRyshCommand
// returns while the script is still running, so an immediate reply would be an
// empty success.
func TestMergeUserCommandResponse(t *testing.T) {
	t.Run("success carries the script output", func(t *testing.T) {
		got := mergeUserCommandResponse(&msg.MsgCLIResponse{OK: true}, "ran\n", nil)
		if !got.OK || got.Output != "ran\n" || got.Error != "" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("failure turns the response red", func(t *testing.T) {
		got := mergeUserCommandResponse(&msg.MsgCLIResponse{OK: true}, "out\n", errors.New("##x exited 3"))
		if got.OK {
			t.Error("a failed script left the response OK")
		}
		if got.Error != "##x exited 3" || got.Output != "out\n" {
			t.Errorf("got %+v", got)
		}
	})

	// A response that was already red never reached a script — an unresolvable
	// pane, an empty command. Overwriting it with a script result that does not
	// exist would turn "we could not run it" into "it worked".
	t.Run("an existing failure survives", func(t *testing.T) {
		orig := &msg.MsgCLIResponse{OK: false, Error: "pane not found: xyz"}
		got := mergeUserCommandResponse(orig, "ignored", nil)
		if got.OK || got.Error != "pane not found: xyz" || got.Output != "" {
			t.Errorf("got %+v", got)
		}
	})

	// Output the dispatch layer printed before the script started is kept, not
	// replaced.
	t.Run("prefix output is preserved", func(t *testing.T) {
		got := mergeUserCommandResponse(&msg.MsgCLIResponse{OK: true, Output: "before\n"}, "after\n", nil)
		if got.Output != "before\nafter\n" {
			t.Errorf("Output = %q", got.Output)
		}
	})
}

// `rysh exec --json` reports status_aware so a script can tell a meaningful 0
// from an uninstrumented one. A user command's status is bash's exit code, so
// it must not be lumped in with the unaudited handlers.
func TestRyshCommandIsStatusAwareForUserCommands(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))

	if RyshCommandIsStatusAware("", "#nfre23") {
		t.Error("a word with no script reported status-aware")
	}
	writeCommand(t, ".rysh", "nfre23", "exit 0\n")
	if !RyshCommandIsStatusAware("", "#nfre23") {
		t.Error("a user command reported not status-aware")
	}
	// Built-ins keep their own audited answer, and the prefix is what selects
	// which lookup runs: a bare word is never a user command.
	if !RyshCommandIsStatusAware("", "help") {
		t.Error("##help lost its statusAware flag")
	}
	if RyshCommandIsStatusAware("", "nfre23") {
		t.Error("a bare word was resolved against .rysh/commands")
	}

	// The lookup is scoped, so the answer depends on which session the command
	// is being sent to — the same word can be a script in one and nothing in
	// another.
	writeSessionCommand(t, ".rysh", "macmini-rysh", "scoped", "exit 0\n")
	if !RyshCommandIsStatusAware("macmini-rysh", "#scoped") {
		t.Error("a session-scoped command reported not status-aware for its own session")
	}
	if RyshCommandIsStatusAware("other-session", "#scoped") {
		t.Error("a session-scoped command reported status-aware for another session")
	}
}

// takePendingUserCommand must hand the handle over exactly once: a second
// caller picking up a stale one would reply to the wrong request.
func TestTakePendingUserCommandClears(t *testing.T) {
	var w WorkspaceActor
	if got := w.takePendingUserCommand(); got != nil {
		t.Fatal("a fresh actor had a pending user command")
	}
	w.pendingUserCommand = func() (string, error) { return "x", nil }
	if got := w.takePendingUserCommand(); got == nil {
		t.Fatal("the handle was not returned")
	}
	if got := w.takePendingUserCommand(); got != nil {
		t.Error("the handle was returned twice")
	}
}

// A CLI command that fails before it dispatches anything must not inherit the
// handle an earlier interactive ##foo left behind: the reply would be held
// until that unrelated script finished.
func TestHandleCLIRyshCommandClearsAStaleHandle(t *testing.T) {
	var w WorkspaceActor
	w.pendingUserCommand = func() (string, error) { return "stale", nil }

	resp := w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{Command: "   "})

	if resp.OK {
		t.Error("an empty command reported success")
	}
	if w.takePendingUserCommand() != nil {
		t.Error("a stale handle survived a command that never dispatched")
	}
}

// The two vocabularies are disjoint, so a file may be called `pane` or `secret`
// and still run — nothing shadows anything. This is what the one-hash prefix
// buys over the first cut, where both lived under ## and the table had to win.
func TestUserCommandsMayShareABuiltinName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))

	var w WorkspaceActor
	if _, ok := lookupRyshCommand("pane"); !ok {
		t.Fatal("##pane is not a built-in any more; this test needs a new name")
	}
	writeCommand(t, ".rysh", "pane", "echo mine\n")

	// "#pane" resolves to the file...
	if word := userCommandLineWord("#pane info"); word != "pane" {
		t.Fatalf("userCommandLineWord(#pane info) = %q", word)
	}
	if _, ok := w.findUserCommand("pane"); !ok {
		t.Error("the file did not resolve as a user command")
	}
	// ...and "##pane" is still the built-in, untouched.
	kind, body := splitRyshPrefix("##pane info")
	if kind != ryshBuiltinCmd || body != "pane info" {
		t.Errorf("splitRyshPrefix(##pane info) = %v, %q", kind, body)
	}

	var out strings.Builder
	if err := w.handleUserCommandsCommand(&out, []string{"list"}); err != nil {
		t.Fatalf("##commands list: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "#pane") || strings.Contains(got, "shadowed") {
		t.Errorf("##commands list = %q, want a plain listing", got)
	}
}

// The shape rule decides which lines rysh may claim at all. It has to be tight:
// "#" opens a comment in shell mode and a heading in a prompt, and rysh takes
// only the form that could not be either.
func TestUserCommandLineWord(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"#nfre23", "nfre23"},
		{"#nfre23 alpha beta", "nfre23"},
		{"#deploy.sh", "deploy.sh"},
		{"#9lives", "9lives"},
		// Not a user command:
		{"##pane info", ""},    // rysh's own prefix, matched earlier anyway
		{"####relay", ""},      // the remote-control escape
		{"##> event", ""},      // a pipeline event
		{"# nfre23", ""},       // a space: an ordinary comment
		{"#!/usr/bin/env", ""}, // a shebang
		{"#-flag", ""},
		{"#", ""},
		{"", ""},
		{"nfre23", ""},
		{"#../etc/passwd", ""}, // could never be joined onto the commands dir
		{"#a/b", ""},
	} {
		if got := userCommandLineWord(tc.line); got != tc.want {
			t.Errorf("userCommandLineWord(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestSplitRyshPrefix(t *testing.T) {
	for _, tc := range []struct {
		line string
		kind ryshCmdKind
		body string
	}{
		{"##pane info", ryshBuiltinCmd, "pane info"},
		{"#nfre23 alpha", ryshUserCmd, "nfre23 alpha"},
		{"# a comment", ryshBuiltinCmd, "# a comment"},
		{"help", ryshBuiltinCmd, "help"},
	} {
		kind, body := splitRyshPrefix(tc.line)
		if kind != tc.kind || body != tc.body {
			t.Errorf("splitRyshPrefix(%q) = %v, %q; want %v, %q", tc.line, kind, body, tc.kind, tc.body)
		}
	}
}

// A CLI body must not be re-prefixed into a line with three hashes, which would
// resolve to neither vocabulary.
func TestRyshCommandLine(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"pane info", "##pane info"},
		{"#nfre23", "#nfre23"},
		{"##pane info", "##pane info"},
	} {
		if got := ryshCommandLine(tc.body); got != tc.want {
			t.Errorf("ryshCommandLine(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A "#" word with no script behind it is only reachable from a CLI caller —
// routeInput leaves an unresolved one to the shell. It must be a failure there,
// not a silent success.
func TestUnknownUserCommandMessage(t *testing.T) {
	t.Chdir(t.TempDir())
	var w WorkspaceActor

	var out strings.Builder
	w.unknownUserCommand(&out, "nfre23")
	got := out.String()
	if !strings.Contains(got, `unknown user command: "nfre23"`) {
		t.Errorf("message %q", got)
	}
	if !strings.Contains(got, filepath.Join(userCommandsSubdir, "nfre23")) {
		t.Errorf("message %q does not name the file to create", got)
	}

	// "#pane" with no file is almost always a miscounted hash, so say so.
	out.Reset()
	w.unknownUserCommand(&out, "pane")
	if got := out.String(); !strings.Contains(got, "##pane is a built-in") {
		t.Errorf("message %q does not mention the built-in", got)
	}
}

// And the mirror: "##nfre23" when a user command of that name exists. The
// prefixes being different is the design, but a user hits this once and needs
// the answer in place.
func TestRyshUnknownCommandPointsAtTheOneHashForm(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	writeCommand(t, ".rysh", "nfre23", "echo hi\n")

	var w WorkspaceActor
	var out strings.Builder
	w.ryshUnknownCommand(&out, "nfre23")

	if got := out.String(); !strings.Contains(got, "#nfre23 is a user command — one hash, not two") {
		t.Errorf("message %q", got)
	}
}
