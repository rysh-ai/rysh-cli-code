// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// native_agent_prompt.go — design 029, amendment C. A ONE-SHOT first prompt for
// `##claude`: `--resume-dir <dir>` and `--prompt-file <path>`.
//
// WHY THIS IS NOT JUST `##claude "$(cat file)"`. That works at the shell — the
// `##` line is split on whitespace and rejoined, so the quotes survive and the
// pane's shell expands the substitution — but every argument after `##claude`
// is ALSO stamped into `agent.args`, and `agent.args` is replayed by both
// restore paths: the daemon-start sweep (nativeResumePlan) and `##ansa resume`.
// A resume prompt typed that way is therefore sent again, as a fresh user turn,
// on every later daemon restart — and once /tmp is wiped it becomes an empty
// `""`. A first prompt is a fact about ONE launch; agent.args is a fact about
// every launch. These flags keep the two apart: the prompt rides on
// nativeAgentSpec.PromptFile, which only the launch path ever sets.
//
// THE FOUNDER'S PROTOCOL (fleet resume): each agent writes
// `.rysh/resumes/<fleet>/<n>/<full pane id>.resume.md` about itself — under the
// SESSION's rysh dir (cfg.RyshDir, the one holding sessions/ and nats/) — and
// `##claude --resume-dir <fleet>/<n>` in that pane starts an EMPTY conversation
// under the pane's own id and hands it that file as its first prompt. An agent restarted with no memory is the
// failure this exists to prevent, so every doubt about the file is a refusal
// that happens BEFORE anything changes — before the old transcript is archived
// and before a shell line is sent.

const (
	// nativeAgentResumeDirFlag names a folder of per-pane resume prompts and
	// implies --fresh-session.
	nativeAgentResumeDirFlag = "--resume-dir"
	// nativeAgentPromptFileFlag names one prompt file, with no other effect on
	// how the agent starts.
	nativeAgentPromptFileFlag = "--prompt-file"
	// nativeAgentResumeSuffix is what the founder's protocol appends to the
	// pane id.
	nativeAgentResumeSuffix = ".resume.md"
	// nativeAgentResumesSubdir is where a relative --resume-dir is rooted,
	// inside the session's rysh dir.
	nativeAgentResumesSubdir = "resumes"

	// nativeAgentPromptMax bounds the prompt. It travels as ONE argv entry
	// (claude's [prompt] positional), and macOS caps the whole argv+env at
	// ARG_MAX (1 MiB). A file past this is far more likely a wrong path than a
	// resume note, and refusing says so where an exec failure would not.
	nativeAgentPromptMax = 256 << 10

	// metaAgentPromptFrom records the file a launch's first prompt came from —
	// audit only. Nothing reads it back to RUN anything; every launch without
	// a prompt flag clears it, so it always describes the latest launch.
	metaAgentPromptFrom = "agent.prompt_from"
)

// nativeAgentPromptRequest is what the two flags asked for.
type nativeAgentPromptRequest struct {
	// ResumeDir is the --resume-dir value; the file inside it is resolved
	// from the pane id.
	ResumeDir string
	// PromptFile is the --prompt-file value.
	PromptFile string
}

func (r nativeAgentPromptRequest) any() bool { return r.ResumeDir != "" || r.PromptFile != "" }

// nativeAgentStripPromptFlags pulls --resume-dir / --prompt-file out of the
// argument list, from any position and in both `--flag value` and
// `--flag=value` forms, for the same reason nativeAgentStripFreshSession does:
// a flag rysh owns must never reach the CLI, which would abort on it.
//
// A flag given twice, given without a value, or both flags together are
// errors rather than last-one-wins: the prompt decides what the agent
// remembers, and a guess about which file was meant is not a safe default.
func nativeAgentStripPromptFlags(args []string) (rest []string, req nativeAgentPromptRequest, err error) {
	rest = make([]string, 0, len(args))
	set := func(dst *string, flag, val string) error {
		val = strings.TrimSpace(val)
		if val == "" || strings.HasPrefix(val, "--") {
			return fmt.Errorf("%s needs a path", flag)
		}
		if *dst != "" {
			return fmt.Errorf("%s given twice", flag)
		}
		*dst = val
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		var dst *string
		var flag string
		switch {
		case a == nativeAgentResumeDirFlag || strings.HasPrefix(a, nativeAgentResumeDirFlag+"="):
			dst, flag = &req.ResumeDir, nativeAgentResumeDirFlag
		case a == nativeAgentPromptFileFlag || strings.HasPrefix(a, nativeAgentPromptFileFlag+"="):
			dst, flag = &req.PromptFile, nativeAgentPromptFileFlag
		default:
			rest = append(rest, args[i])
			continue
		}
		val := ""
		if eq := strings.IndexByte(a, '='); eq > 0 {
			val = a[eq+1:]
		} else if i+1 < len(args) {
			i++
			val = args[i]
		}
		if err := set(dst, flag, val); err != nil {
			return nil, nativeAgentPromptRequest{}, err
		}
	}
	if req.ResumeDir != "" && req.PromptFile != "" {
		return nil, nativeAgentPromptRequest{}, fmt.Errorf("%s and %s are mutually exclusive — pick the one file the agent starts from",
			nativeAgentResumeDirFlag, nativeAgentPromptFileFlag)
	}
	return rest, req, nil
}

// nativeAgentPromptPath turns the request into one absolute file path.
//
// A RELATIVE --resume-dir is rooted at `<ryshDir>/resumes/` — the session's
// own rysh dir, which is the same directory for every pane of the session and
// is never a guess. That is what lets the founder type `--resume-dir wayto/1`.
// It may not climb out of that root (`..`): the flag names a resume folder,
// not an arbitrary file.
//
// An absolute (or `~/…`) --resume-dir is taken as given, so a folder outside
// the session still works. A RELATIVE --prompt-file is refused: it would
// resolve against the pane's cwd, which is sometimes a guess (nativeAgentCwd's
// "daemon" fallback) — a prompt read from the wrong directory is the exact
// silent failure this feature exists to rule out.
//
// The pane id is the daemon's own knowledge, not `$RYSH_PANE`: the flag works
// identically whether it was typed in the pane or sent to it from elsewhere.
func nativeAgentPromptPath(req nativeAgentPromptRequest, paneID, ryshDir string) (string, error) {
	raw, flag := req.PromptFile, nativeAgentPromptFileFlag
	if req.ResumeDir != "" {
		raw, flag = req.ResumeDir, nativeAgentResumeDirFlag
	}
	p, err := expandHome(raw)
	if err != nil {
		return "", fmt.Errorf("%s %s: %v", flag, raw, err)
	}
	if !filepath.IsAbs(p) {
		if req.ResumeDir == "" {
			return "", fmt.Errorf("%s %s: give an absolute path (or ~/…)", flag, raw)
		}
		if strings.TrimSpace(ryshDir) == "" {
			return "", fmt.Errorf("%s %s: this session has no rysh dir to resolve it in — give an absolute path", flag, raw)
		}
		root := filepath.Join(ryshDir, nativeAgentResumesSubdir)
		p = filepath.Join(root, p)
		if rel, rerr := filepath.Rel(root, p); rerr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s %s: must name a folder inside %s", flag, raw, root)
		}
	}
	if req.ResumeDir != "" {
		if strings.TrimSpace(paneID) == "" || strings.ContainsAny(paneID, `/\`) {
			return "", fmt.Errorf("%s: this pane has no usable id", flag)
		}
		p = filepath.Join(p, paneID+nativeAgentResumeSuffix)
	}
	return filepath.Clean(p), nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// readNativeAgentPrompt reads the prompt and refuses anything an agent should
// not be started from: a missing file, a directory, an empty or
// whitespace-only file, or one too large to pass as an argument. The error
// always names the path it looked at — "file not found" with no path is how a
// typo in the folder name turns into a wasted restart.
func readNativeAgentPrompt(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no prompt file at %s", path)
		}
		return nil, fmt.Errorf("cannot read %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > nativeAgentPromptMax {
		return nil, fmt.Errorf("%s is %d bytes; the limit is %d", path, fi.Size(), nativeAgentPromptMax)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", path, err)
	}
	defer f.Close()
	// Read through a limit too: the size above is a stat, and a file that
	// grows between the stat and the read must not slip past the cap.
	data, err := io.ReadAll(io.LimitReader(f, nativeAgentPromptMax+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", path, err)
	}
	if len(data) > nativeAgentPromptMax {
		return nil, fmt.Errorf("%s is over the %d-byte limit", path, nativeAgentPromptMax)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, fmt.Errorf("%s is empty — refusing to start an agent with no memory", path)
	}
	return data, nil
}

// nativeAgentPromptStageDir is where one-shot copies wait for the pane shell to
// read them. Under the daemon's own temp dir (per-user on macOS), 0700.
func nativeAgentPromptStageDir() string {
	return filepath.Join(os.TempDir(), "rysh-oneshot-prompts")
}

// stageNativeAgentPrompt writes a private copy of the prompt for the launch
// line to consume.
//
// A COPY, NOT THE ORIGINAL PATH. The launch reads its prompt a moment after
// the checks above, in another process; the copy makes what was checked
// exactly what is sent, whatever happens to the source in between — an agent
// rewriting its resume note, or a cleanup of /tmp. The copy is 0600 in a 0700
// directory because a resume note describes the agent's work and may quote
// anything it saw. The launch line deletes it as it reads it
// (nativeAgentWithPrompt), so nothing persists past a successful start.
func stageNativeAgentPrompt(dir, paneID string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll keeps the mode of a directory that already exists. Tighten it
	// rather than trust whoever made it first.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, paneID+"-*.md") // CreateTemp creates 0600
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// nativeAgentPromptVar holds the prompt between reading it and launching.
const nativeAgentPromptVar = "__rysh_prompt"

// nativeAgentWithPrompt wraps a launch line so the agent starts with the staged
// copy's content as its [prompt] positional, and the copy is removed as it is
// read.
//
// FAIL CLOSED AT THE SHELL TOO. A bare `"$(cat f)"` that fails still runs
// claude — with an empty prompt, which is an agent with no memory, the one
// outcome this feature must never produce. Reading into a variable first and
// chaining with `&&` means a read that fails starts nothing and leaves cat's
// error on the pane's screen. The variable is unset once the agent exits.
func nativeAgentWithPrompt(cmd, path string) string {
	q := shellQuote(path)
	v := nativeAgentPromptVar
	// cmd already carries the "$v" argument (nativeAgentCommand puts it inside
	// the agent command, so both branches of the scrubbed line get it).
	return v + `="$(cat -- ` + q + `)" && rm -f -- ` + q + ` && ` + cmd + `; unset ` + v
}
