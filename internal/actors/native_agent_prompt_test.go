// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Design 029 amendment C — the one-shot first prompt (`--resume-dir`,
// `--prompt-file`). The property everything here protects: an agent restarted
// for a fresh context starts WITH the note it wrote about itself, exactly once,
// or does not start at all.

const promptTestPane = "99bf15c0-e19a-4acd-b757-352daaabe82b"

func TestPromptFlagsAreStrippedFromAnyPositionAndForm(t *testing.T) {
	cases := []struct {
		args     []string
		dir      string
		file     string
		wantRest []string
	}{
		{[]string{"--resume-dir", "/tmp/wayto/resume/1/"}, "/tmp/wayto/resume/1/", "", nil},
		{[]string{"--model", "opus", "--resume-dir=/r"}, "/r", "", []string{"--model", "opus"}},
		{[]string{"--prompt-file", "/p.md", "--model", "opus"}, "", "/p.md", []string{"--model", "opus"}},
		{[]string{"--prompt-file=/p.md"}, "", "/p.md", nil},
	}
	for _, c := range cases {
		rest, req, err := nativeAgentStripPromptFlags(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if req.ResumeDir != c.dir || req.PromptFile != c.file {
			t.Errorf("%v: got %+v", c.args, req)
		}
		if strings.Join(rest, " ") != strings.Join(c.wantRest, " ") {
			t.Errorf("%v: rest = %v, want %v — a rysh flag must never reach the CLI", c.args, rest, c.wantRest)
		}
	}
}

// Every ambiguity about WHICH file is a refusal, never a guess.
func TestAmbiguousPromptFlagsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"--resume-dir"},
		{"--resume-dir="},
		{"--resume-dir", "--model"},
		{"--prompt-file", "/a", "--prompt-file", "/b"},
		{"--resume-dir", "/r", "--prompt-file", "/p"},
	} {
		if _, _, err := nativeAgentStripPromptFlags(args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	// A near-miss belongs to somebody else and passes through untouched.
	rest, req, err := nativeAgentStripPromptFlags([]string{"--resume-dirs", "x"})
	if err != nil || req.any() || len(rest) != 2 {
		t.Errorf("near-miss was swallowed: rest=%v req=%+v err=%v", rest, req, err)
	}
}

// The founder's protocol: `--resume-dir wayto/1` reads
// <session rysh dir>/resumes/wayto/1/<full pane id>.resume.md, named by the id
// the DAEMON knows — no $RYSH_PANE expansion involved.
func TestARelativeResumeDirIsRootedInTheSessionsResumes(t *testing.T) {
	const rysh = "/work/rysh/.rysh"
	got, err := nativeAgentPromptPath(nativeAgentPromptRequest{ResumeDir: "wayto/1"}, promptTestPane, rysh)
	if err != nil {
		t.Fatal(err)
	}
	if want := rysh + "/resumes/wayto/1/" + promptTestPane + ".resume.md"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// The flag names a resume folder, not an arbitrary file: no climbing out of
// .rysh/resumes, and no guessing when the session has no rysh dir.
func TestARelativeResumeDirCannotEscapeOrGuess(t *testing.T) {
	for _, dir := range []string{"..", "../secrets", "wayto/../../x", "."} {
		if p, err := nativeAgentPromptPath(nativeAgentPromptRequest{ResumeDir: dir}, promptTestPane, "/work/.rysh"); err == nil {
			t.Errorf("%q: accepted as %s", dir, p)
		}
	}
	if _, err := nativeAgentPromptPath(nativeAgentPromptRequest{ResumeDir: "wayto/1"}, promptTestPane, ""); err == nil {
		t.Error("a relative dir was resolved with no session rysh dir")
	}
}

// An absolute folder is still taken as given.
func TestAnAbsoluteResumeDirIsTakenAsGiven(t *testing.T) {
	got, err := nativeAgentPromptPath(nativeAgentPromptRequest{ResumeDir: "/tmp/wayto/resume/1/"}, promptTestPane, "/work/.rysh")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/tmp/wayto/resume/1/" + promptTestPane + ".resume.md"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	home, _ := os.UserHomeDir()
	got, err = nativeAgentPromptPath(nativeAgentPromptRequest{PromptFile: "~/notes/p.md"}, promptTestPane, "")
	if err != nil || got != filepath.Join(home, "notes", "p.md") {
		t.Errorf("~ not expanded: %s %v", got, err)
	}
}

// A relative prompt FILE would resolve against a cwd that is sometimes a guess.
func TestARelativePromptFileIsRefused(t *testing.T) {
	if _, err := nativeAgentPromptPath(nativeAgentPromptRequest{PromptFile: "p.md"}, promptTestPane, "/work/.rysh"); err == nil {
		t.Error("relative --prompt-file accepted")
	}
}

// NEVER START AN AGENT WITH NO MEMORY. Each of these would have done so.
func TestUnusablePromptFilesAreRefusedByName(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	blank := filepath.Join(dir, "blank.md")
	big := filepath.Join(dir, "big.md")
	missing := filepath.Join(dir, "missing.md")
	os.WriteFile(empty, nil, 0o600)
	os.WriteFile(blank, []byte(" \n\t\n"), 0o600)
	os.WriteFile(big, []byte(strings.Repeat("x", nativeAgentPromptMax+1)), 0o600)

	for _, p := range []string{empty, blank, big, missing, dir} {
		_, err := readNativeAgentPrompt(p)
		if err == nil {
			t.Errorf("%s: accepted", p)
			continue
		}
		// The path in the message is what makes a typo fixable on the spot.
		if !strings.Contains(err.Error(), p) {
			t.Errorf("%s: error does not name the path: %v", p, err)
		}
	}

	ok := filepath.Join(dir, "ok.md")
	os.WriteFile(ok, []byte("You are W5. Carry on from step 3."), 0o600)
	if data, err := readNativeAgentPrompt(ok); err != nil || !strings.Contains(string(data), "step 3") {
		t.Errorf("a good file was refused: %v", err)
	}
}

// A resume note may quote anything the agent saw: the copy is private, and a
// stage dir someone else created loosely is tightened, not trusted.
func TestTheStagedCopyIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := stageNativeAgentPrompt(dir, promptTestPane, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("stage dir mode %v, want 0700", fi.Mode().Perm())
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged file mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Errorf("staged content %q", b)
	}
}

// THE SHELL LINE, RUN FOR REAL against a stand-in claude that records its argv.
// Three properties only an execution can show: the prompt arrives as ONE
// argument, byte for byte (quotes, $, backticks and newlines included); the
// staged copy is gone after the read; and a copy that cannot be read starts
// nothing at all rather than an agent with an empty prompt.
func TestThePromptLineDeliversOnceAndFailsClosed(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "sh"} {
		shPath, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		t.Run(sh, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.MkdirAll(bin, 0o755)
			argv := filepath.Join(dir, "argv")
			// Records the last argument (the prompt) exactly, NUL-free.
			fake := "#!/bin/sh\nfor a; do last=$a; done\nprintf '%s' \"$last\" > " + shellQuote(argv) + "\n"
			os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o755)

			prompt := "You are W5 — it's step 3.\nDon't run `rm`; $HOME stays \"put\".\n"
			staged, err := stageNativeAgentPrompt(filepath.Join(dir, "stage"), promptTestPane, []byte(prompt))
			if err != nil {
				t.Fatal(err)
			}
			line := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: promptTestPane, PromptFile: staged})

			run := func() {
				cmd := exec.Command(shPath, "-c", line)
				cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
				_ = cmd.Run() // a failed read is EXPECTED to exit non-zero below
			}
			run()
			got, err := os.ReadFile(argv)
			if err != nil {
				t.Fatalf("claude did not run: %v\nline: %s", err, line)
			}
			// $(…) strips trailing newlines, as every shell does; the rest is exact.
			if string(got) != strings.TrimRight(prompt, "\n") {
				t.Errorf("prompt arrived altered:\n got %q\nwant %q", got, prompt)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Errorf("staged copy survived the launch: %v", err)
			}

			// Second run: the copy is gone. Nothing may start.
			os.Remove(argv)
			run()
			if _, err := os.Stat(argv); !os.IsNotExist(err) {
				t.Errorf("claude started although its prompt could not be read")
			}
		})
	}
}

// THE BUG THIS FIXES. What the launch stamps is what both restore paths read
// back, and neither may find the prompt there: not the daemon-start sweep, not
// `##ansa resume`.
func TestTheFirstPromptIsNeverStoredOrReplayed(t *testing.T) {
	const from = "/tmp/wayto/resume/1/" + promptTestPane + ".resume.md"
	meta := map[string]string{}
	for _, kv := range nativeAgentMeta(nativeAgentClaude, "/work", "--model opus", promptTestPane, true, from) {
		meta[kv[0]] = kv[1]
	}
	if meta[metaAgentArgs] != "--model opus" {
		t.Errorf("agent.args = %q, want only the user's flags", meta[metaAgentArgs])
	}
	if meta[metaAgentPromptFrom] != from {
		t.Errorf("agent.prompt_from = %q, want the audit path", meta[metaAgentPromptFrom])
	}

	panes := []ansaPane{{ID: promptTestPane, Meta: meta}}
	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("pane not resumed: %+v", deliveries)
	}
	replays := map[string]string{
		"daemon-start sweep": deliveries[0].Command,
		"##ansa resume":      ansaResumeCommand("claude", promptTestPane, ansaPaneAgentArgs(panes[0])),
	}
	for path, cmd := range replays {
		for _, leak := range []string{"resume.md", nativeAgentPromptVar, "cat --", "rysh-oneshot"} {
			if strings.Contains(cmd, leak) {
				t.Errorf("%s replays the first prompt (%q): %s", path, leak, cmd)
			}
		}
	}
}

// A launch without a prompt flag CLEARS the audit key (an empty value deletes
// it), so it never describes a launch from three launches ago.
func TestALaunchWithoutAPromptClearsTheAuditKey(t *testing.T) {
	for _, kv := range nativeAgentMeta(nativeAgentClaude, "/work", "", promptTestPane, false, "") {
		if kv[0] == metaAgentPromptFrom {
			if kv[1] != "" {
				t.Errorf("stale prompt_from stamped: %q", kv[1])
			}
			return
		}
	}
	t.Error("agent.prompt_from is not stamped at all, so an old value would survive")
}

// The refusals happen BEFORE the pane is touched. This drives the real handler
// on a WorkspaceActor with no transport at all: reaching the foreground probe
// would panic, so returning an error proves the check ran first — and the
// transcript under the pane id is still where it was.
func TestARefusedResumeChangesNothing(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := filepath.Join(cfg, "projects", "-work")
	os.MkdirAll(proj, 0o755)
	transcript := filepath.Join(proj, promptTestPane+".jsonl")
	os.WriteFile(transcript, []byte("{}\n"), 0o600)

	ryshDir := t.TempDir()
	resumeDir := "wayto/1" // relative: resolved under <rysh dir>/resumes/
	os.MkdirAll(filepath.Join(ryshDir, "resumes", resumeDir), 0o700)
	emptyFile := filepath.Join(ryshDir, "resumes", resumeDir, promptTestPane+".resume.md")

	w := &WorkspaceActor{cfg: config.Config{RyshDir: ryshDir}}
	for name, tc := range map[string]struct {
		kind  string
		args  []string
		setup func()
	}{
		"missing file": {nativeAgentClaude, []string{"--resume-dir", resumeDir}, func() { os.Remove(emptyFile) }},
		"empty file":   {nativeAgentClaude, []string{"--resume-dir", resumeDir}, func() { os.WriteFile(emptyFile, nil, 0o600) }},
		"with new":     {nativeAgentClaude, []string{"new", "--resume-dir", resumeDir}, func() {}},
		"codex":        {nativeAgentCodex, []string{"--resume-dir", resumeDir}, func() {}},
	} {
		tc.setup()
		var out strings.Builder
		err := w.handleNativeAgentCommand(&out, promptTestPane, tc.kind, tc.args)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, serr := os.Stat(transcript); serr != nil {
			t.Fatalf("%s: the old transcript was moved by a refused launch: %v", name, serr)
		}
	}

	var out strings.Builder
	os.Remove(emptyFile)
	w.handleNativeAgentCommand(&out, promptTestPane, nativeAgentClaude, []string{"--resume-dir", resumeDir})
	if !strings.Contains(out.String(), emptyFile) || !strings.Contains(out.String(), "nothing was changed") {
		t.Errorf("the refusal must name the file it looked for and say nothing changed:\n%s", out.String())
	}
}
