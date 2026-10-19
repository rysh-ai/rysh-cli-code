// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// ##cmd --capture: writing the file, and reading it back
//
// --capture exists because a pane's output is a rendered terminal, not a
// stream: once a full-screen program has drawn over it there is nothing left to
// read, and even at a prompt the text is wrapped to the pane's width. The file
// is the only faithful copy.
//
// The read-back has to live here too, in the daemon. It used to be missing, and
// the manifest's "poll each file for the sentinel" left `##cmd` as the only
// tool in reach — so the poll went out as ANOTHER broadcast of `cat <file>`,
// typed into every pane in scope. The panes running claude took it as a prompt,
// answered something that was not the sentinel, and the poller retried: 64
// pane-writes in two minutes, interleaved with what the operator was typing.
// A read that touches no pane is the whole point of this file.
// ---------------------------------------------------------------------------

// captureSentinel ends a captured run. The exit status follows it on the same
// line.
const captureSentinel = "__rysh_capture_done:"

// captureReadLimit caps what `##cmd capture <pane>` prints from one file. A
// captured `make test` is megabytes, and the reader is usually an LLM whose
// context it would fill. Past the limit the TAIL is printed — the end is where
// a failure reports itself — and the note says so; the path is always printed,
// so the whole file stays one `Read` away.
const captureReadLimit = 32 << 10

// captureCommand rewrites a command so its output lands in a file instead of
// only on the pane's screen, and appends a sentinel line carrying the exit
// status so a reader can tell "still running" from "finished, no output".
//
// The wait happens in the CALLER, deliberately. A ## command runs on the
// workspace's mailbox goroutine, which every other pane's commands queue behind
// — blocking it until N shells finish would freeze the session, which is
// exactly the defect an audit of `##proxy check` found in this codebase.
// Reading a finished file back (handleCmdCapture) is a local stat and read, so
// that one does run on the mailbox.
func captureCommand(baseDir, paneID, command string) string {
	path := capturePath(baseDir, paneID)
	// { … ; } groups without a subshell, so `cd` and exports behave as they
	// would unredirected. $? is read immediately after, before anything else
	// can overwrite it.
	return fmt.Sprintf("mkdir -p %s && { %s ; } > %s 2>&1; echo \"%s$?\" >> %s",
		filepath.Dir(path), command, path, captureSentinel, path)
}

func captureDir(baseDir string) string {
	return filepath.Join(baseDir, ".rysh", "captures")
}

func capturePath(baseDir, paneID string) string {
	return filepath.Join(captureDir(baseDir), paneID+".out")
}

// ensureCaptureDir creates the capture directory and makes it invisible to git.
//
// Captures are transient output that lands inside the user's project, because
// that is where rysh keeps its state. Without this they show up as untracked
// files and ride along in the next `git add -A` — someone's build log committed
// as source. A directory that ignores itself is the smallest fix that survives
// being copied, moved or recreated.
func ensureCaptureDir(baseDir string) {
	dir := captureDir(baseDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); err == nil {
		return
	}
	_ = os.WriteFile(ignore, []byte("# rysh capture output — transient\n*\n"), 0o600)
}

// capture is one pane's capture file, already classified.
type capture struct {
	paneID string
	path   string
	// body is the captured output with the sentinel line removed.
	body string
	// done reports that the sentinel was found, which is the only way to tell
	// "finished with no output" from "still running".
	done bool
	// status is the command's exit status; meaningful only when done.
	status int
	// truncated reports that body is the tail of a longer file.
	truncated int64
}

// parseCapture splits a capture file into its output and its sentinel.
//
// The sentinel is not reliably on a line of its own: `echo` supplies its own
// newline, but a command whose output does not end in one leaves the file as
// `…last line__rysh_capture_done:0`. Scanning for the last occurrence and
// requiring nothing but the status after it handles both, and a sentinel-shaped
// string inside the output itself is harmless — the real one is still last.
func parseCapture(content string) (body string, done bool, status int) {
	i := strings.LastIndex(content, captureSentinel)
	if i < 0 {
		return content, false, 0
	}
	line, rest, _ := strings.Cut(content[i+len(captureSentinel):], "\n")
	if strings.TrimSpace(rest) != "" {
		// Something followed the sentinel, so it is not the end of this run.
		return content, false, 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return content, false, 0
	}
	return content[:i], true, n
}

// readCapture loads and classifies one capture file.
func readCapture(path, paneID string) (capture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return capture{}, err
	}
	c := capture{paneID: paneID, path: path}
	c.body, c.done, c.status = parseCapture(string(b))
	if n := int64(len(c.body)) - captureReadLimit; n > 0 {
		c.truncated = n
		c.body = c.body[n:]
	}
	return c, nil
}

// listCaptures returns every capture in the directory, newest last.
func listCaptures(baseDir string) ([]capture, error) {
	dir := captureDir(baseDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []capture
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".out") {
			continue
		}
		c, err := readCapture(filepath.Join(dir, name), strings.TrimSuffix(name, ".out"))
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].paneID < out[j].paneID })
	return out, nil
}

// resolveCapturePaneID accepts the full pane id the --capture manifest printed,
// or any unambiguous prefix of one. Prefixes matter because the ids are UUIDs
// and every other rysh selector takes a short form; without it the only way to
// name a capture is to copy 36 characters exactly.
func resolveCapturePaneID(captures []capture, arg string) (string, error) {
	var hits []string
	for _, c := range captures {
		if c.paneID == arg {
			return c.paneID, nil
		}
		if strings.HasPrefix(c.paneID, arg) {
			hits = append(hits, c.paneID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("no capture for pane %q (##cmd capture lists the ones there are)", arg)
	default:
		return "", fmt.Errorf("pane %q is ambiguous: %s", arg, strings.Join(hits, ", "))
	}
}

// captureState renders the classification as the one word a caller branches on.
func (c capture) state() string {
	if !c.done {
		return "running"
	}
	return fmt.Sprintf("exit %d", c.status)
}

// handleCmdCapture implements `##cmd capture [<pane>]`: the read-back for
// --capture, served from the daemon so that reading a result never types
// anything into a pane.
func (w *WorkspaceActor) handleCmdCapture(out *strings.Builder, args []string) {
	captures, err := listCaptures(w.baseDir())
	if err != nil || len(captures) == 0 {
		w.failRysh("##cmd capture: no captures in %s", captureDir(w.baseDir()))
		fmt.Fprintf(out, "\n[rysh] ##cmd capture: nothing captured yet (%s)\n",
			captureDir(w.baseDir()))
		return
	}

	if len(args) == 0 {
		fmt.Fprintf(out, "\n[rysh] ##cmd capture: %d capture(s) in %s\n",
			len(captures), captureDir(w.baseDir()))
		for _, c := range captures {
			fmt.Fprintf(out, "  %-9s %s  %d byte(s)\n", c.state(), c.paneID, len(c.body))
		}
		fmt.Fprintf(out, "  ##cmd capture <pane>  print one\n")
		return
	}

	paneID, err := resolveCapturePaneID(captures, args[0])
	if err != nil {
		w.failRyshUsage("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}
	var c capture
	for _, e := range captures {
		if e.paneID == paneID {
			c = e
			break
		}
	}

	fmt.Fprintf(out, "\n[rysh] ##cmd capture %s: %s\n", paneID, c.state())
	fmt.Fprintf(out, "  %s\n", c.path)
	if c.truncated > 0 {
		fmt.Fprintf(out, "  (tail only: %d earlier byte(s) omitted — read the file for all of it)\n",
			c.truncated)
	}
	if !c.done {
		// Still running is a legitimate answer, not a failure: the caller polls
		// again. Saying so beats handing back a half-written file as if it were
		// the result.
		fmt.Fprintf(out, "  still running — no %s<status> line yet\n", captureSentinel)
	}
	if c.body != "" {
		fmt.Fprintf(out, "%s", c.body)
		if !strings.HasSuffix(c.body, "\n") {
			fmt.Fprintf(out, "\n")
		}
	}
}
