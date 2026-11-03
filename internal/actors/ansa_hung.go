// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `ansa hung` — is the claude in a pane hung? (`rysh ansa hung`, `##ansa hung`)
//
// Why it exists (2026-11-01 01:41Z): a claude hung mid-turn and seven messages,
// one of them a founder go-ahead, sat unanswered until the founder pressed Esc.
// The screen alone could not show it: the spinner keeps animating while the API
// call hangs. What does show it is a working claude whose transcript file has
// stopped changing.
//
// WHAT IT READS, AND ONLY THAT (founder ruling D-67 (a), 2026-11-01):
//
//   - the transcript's FILE TIME, by os.Stat. The file is never opened, so no
//     line of it is read, counted or parsed. Its folder under projects/ is
//     found by listing folder NAMES, not by reading anything;
//   - the pane's screen, for claude's working marker (ansaScreenIsWorking).
//
// HUNG = the working marker is on screen AND the transcript has not changed for
// N minutes (default 10). Either alone is not HUNG: a busy claude writes its
// transcript as it goes, and an idle claude's transcript stops moving too.
//
// It is read-only: it presses no key and writes no meta. Freeing a hung claude
// is a separate, audited step (`rysh ansa keys <pane-id> -- esc`), taken by a
// person or agent who has looked at the pane.
//
// LIMITS, stated in every report:
//   - a pane under ~50 columns hides the marker, so it reads NO-MARKER, never
//     HUNG (ansaScreenIsWorking explains the width problem);
//   - the transcript is found from the pane's claude.session_id meta. If that
//     stamp is stale (the claude moved to a new session in that pane), the old
//     file stops changing and a working claude looks HUNG.

const (
	ansaHungDefaultMinutes = 10
	ansaHungMaxMinutes     = 24 * 60
	// ansaHungParallel bounds concurrent screen reads for --all. Each read can
	// take up to ansaProbeTimeout on a wedged pane, and the CLI waits 30 s in
	// all, so they cannot run one after another across a big session.
	ansaHungParallel = 8
)

// Verdicts, padded to one width in the report.
const (
	ansaHungHung     = "HUNG"
	ansaHungBusy     = "BUSY"
	ansaHungNoMarker = "NO-MARKER"
	ansaHungUnknown  = "UNKNOWN"
)

// ansaClaudeSessionIDRe is the only shape a claude.session_id may have before
// it is used in a file name. Meta is written by anything that can reach the
// session bus, so "../../x" or "*" must never become part of a path.
var ansaClaudeSessionIDRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type ansaHungRequest struct {
	To            string
	ClaudeSession string
	All           bool
	Minutes       int
}

// ansaHungEnv is what the check reads from outside the session: where claude
// keeps its files, and the clock. A seam so tests never touch the real
// ~/.claude.
type ansaHungEnv struct {
	ClaudeHome string
	// ClaudeHomeLabel is how notes name ClaudeHome ("~/.claude" or
	// "$CLAUDE_CONFIG_DIR"): the absolute path carries the username, so it is
	// never printed (LEAD note N1).
	ClaudeHomeLabel string
	Now             func() time.Time
}

// ansaHungRow is the verdict for one pane.
type ansaHungRow struct {
	PaneID    string
	Persona   string
	SessionID string
	Verdict   string
	Working   bool          // the working marker was on screen
	Found     bool          // a transcript file was found
	Age       time.Duration // time since the transcript last changed
	Note      string        // why the verdict is UNKNOWN
}

// ansaClaudeHome is where claude keeps its files for the daemon's user
// ($CLAUDE_CONFIG_DIR when set, else ~/.claude), and the label notes print
// instead of the absolute path.
func ansaClaudeHome() (dir, label string) {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d, "$CLAUDE_CONFIG_DIR"
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", "~/.claude"
	}
	return filepath.Join(h, ".claude"), "~/.claude"
}

// ansaHungEnvNow is the env the two doors use.
func ansaHungEnvNow() ansaHungEnv {
	dir, label := ansaClaudeHome()
	return ansaHungEnv{ClaudeHome: dir, ClaudeHomeLabel: label}
}

// ansaHungCheck resolves the target(s) and returns one row per claude pane.
// A refusal means nothing was checked.
func ansaHungCheck(tr ansaTransport, req ansaHungRequest, env ansaHungEnv) ([]ansaHungRow, *msg.MsgAnsaRouteResult) {
	to := strings.TrimSpace(req.To)
	sid := strings.TrimSpace(req.ClaudeSession)
	n := 0
	for _, set := range []bool{to != "", sid != "", req.All} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, msg.AnsaRefusal(msg.AnsaErrNoTarget,
			"name ONE of: a pane id, an @given-name, --claude-session <id>, or --all")
	}
	minutes := req.Minutes
	if minutes == 0 {
		minutes = ansaHungDefaultMinutes
	}
	if minutes < 1 || minutes > ansaHungMaxMinutes {
		return nil, msg.AnsaRefusal(msg.AnsaErrBadMinutes,
			"--minutes must be between 1 and %d, got %d", ansaHungMaxMinutes, req.Minutes)
	}

	panes, err := tr.Panes()
	if err != nil {
		return nil, msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so nothing was checked: %v", err)
	}
	var targets []ansaPane
	switch {
	case req.All:
		for _, p := range panes {
			if strings.TrimSpace(p.meta(metaClaudeSessionID)) != "" {
				targets = append(targets, p)
			}
		}
	case sid != "":
		p, refusal := ansaResolveClaudeSession(panes, sid)
		if refusal != nil {
			return nil, refusal
		}
		targets = []ansaPane{p}
	default:
		p, refusal := ansaResolveTarget(panes, to)
		if refusal != nil {
			return nil, refusal
		}
		targets = []ansaPane{p}
	}

	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	limit := time.Duration(minutes) * time.Minute
	rows := make([]ansaHungRow, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, ansaHungParallel)
	for i, p := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p ansaPane) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = ansaHungOne(tr, p, env, now, limit)
		}(i, p)
	}
	wg.Wait()
	return rows, nil
}

// ansaHungOne builds the row for one pane.
func ansaHungOne(tr ansaTransport, p ansaPane, env ansaHungEnv, now func() time.Time, limit time.Duration) ansaHungRow {
	row := ansaHungRow{PaneID: p.ID, Persona: ansaPersona(p), Verdict: ansaHungUnknown}
	sid := strings.TrimSpace(p.meta(metaClaudeSessionID))
	row.SessionID = sid
	if sid == "" {
		row.Note = "no " + metaClaudeSessionID + " meta on this pane, so its transcript cannot be found"
		return row
	}
	if !ansaClaudeSessionIDRe.MatchString(sid) {
		row.SessionID = ""
		row.Note = metaClaudeSessionID + " on this pane is not a claude session id, so it is not used as a file name"
		return row
	}

	rows, err := tr.Screen(p.ID)
	if err != nil {
		row.Note = fmt.Sprintf("the screen could not be read: %v", err)
		return row
	}
	row.Working = ansaScreenIsWorking(rows)

	label := env.ClaudeHomeLabel
	if label == "" {
		label = "the claude home"
	}
	mtime, found, err := ansaTranscriptMtime(env.ClaudeHome, sid)
	if err != nil {
		// The error's own text carries the absolute path; print only why.
		why := err
		var pe *fs.PathError
		if errors.As(err, &pe) {
			why = pe.Err
		}
		row.Note = fmt.Sprintf("%s/projects could not be listed: %v", label, why)
		return row
	}
	if !found {
		row.Note = "no transcript file for this claude session under " + label + "/projects"
		return row
	}
	row.Found = true
	row.Age = now().Sub(mtime)
	if row.Age < 0 {
		row.Age = 0
	}
	switch {
	case row.Working && row.Age >= limit:
		row.Verdict = ansaHungHung
	case row.Working:
		row.Verdict = ansaHungBusy
	default:
		row.Verdict = ansaHungNoMarker
	}
	return row
}

// ansaTranscriptMtime returns the newest modification time of
// <home>/projects/<any folder>/<sid>.jsonl. It lists folder names and stats
// files; it never opens a transcript. sid must already match
// ansaClaudeSessionIDRe (checked again here, so no caller can skip it).
func ansaTranscriptMtime(home, sid string) (time.Time, bool, error) {
	if home == "" || !ansaClaudeSessionIDRe.MatchString(sid) {
		return time.Time{}, false, nil
	}
	root := filepath.Join(home, "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	var newest time.Time
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := os.Stat(filepath.Join(root, e.Name(), sid+".jsonl"))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if !found || fi.ModTime().After(newest) {
			newest, found = fi.ModTime(), true
		}
	}
	return newest, found, nil
}

// ansaHungReport renders rows for a person or a log.
func ansaHungReport(rows []ansaHungRow, minutes int) string {
	if minutes == 0 {
		minutes = ansaHungDefaultMinutes
	}
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("no pane in this session carries " + metaClaudeSessionID + " meta; nothing to check\n")
		return b.String()
	}
	count := map[string]int{}
	for _, r := range rows {
		count[r.Verdict]++
	}
	fmt.Fprintf(&b, "%d claude panes checked: %d hung, %d busy, %d no marker, %d unknown\n",
		len(rows), count[ansaHungHung], count[ansaHungBusy], count[ansaHungNoMarker], count[ansaHungUnknown])
	for _, r := range rows {
		fmt.Fprintf(&b, "%-10s %s (pane %s): ", r.Verdict, r.Persona, r.PaneID)
		switch r.Verdict {
		case ansaHungHung:
			fmt.Fprintf(&b, "working marker on screen; transcript unchanged for %s", ansaHungAge(r.Age))
		case ansaHungBusy:
			fmt.Fprintf(&b, "working marker on screen; transcript written %s ago", ansaHungAge(r.Age))
		case ansaHungNoMarker:
			fmt.Fprintf(&b, "no working marker on screen; transcript written %s ago", ansaHungAge(r.Age))
		default:
			b.WriteString(r.Note)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "HUNG = claude's working marker is on screen and its transcript file has not changed for %d min.\n", minutes)
	b.WriteString("Reads file times and the screen only, never transcript content (D-67).\n")
	b.WriteString("Limits: a pane under ~50 columns hides the marker (NO-MARKER); a stale " + metaClaudeSessionID +
		" makes a working claude look HUNG.\n")
	b.WriteString("To free a hung claude after looking at it: rysh ansa keys <pane-id> -- esc\n")
	return b.String()
}

// ansaHungAge renders 20s, 14m, 3h05m.
func ansaHungAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

// parseAnsaHungArgs reads `##ansa hung` arguments:
//
//	<@name|pane-id> [--minutes N]
//	--claude-session <id> [--minutes N]
//	--all [--minutes N]
func parseAnsaHungArgs(args []string) (ansaHungRequest, error) {
	var req ansaHungRequest
	value := func(i int, flag string) (string, error) {
		if i+1 >= len(args) || strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return strings.TrimSpace(args[i+1]), nil
	}
	minutes := func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ansaHungMaxMinutes {
			return fmt.Errorf("--minutes must be a whole number from 1 to %d, got %q", ansaHungMaxMinutes, v)
		}
		req.Minutes = n
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		switch {
		case a == "":
		case a == "--all":
			req.All = true
		case a == "--claude-session":
			v, err := value(i, a)
			if err != nil {
				return req, err
			}
			req.ClaudeSession = v
			i++
		case strings.HasPrefix(a, "--claude-session="):
			req.ClaudeSession = strings.TrimSpace(strings.TrimPrefix(a, "--claude-session="))
			if req.ClaudeSession == "" {
				return req, fmt.Errorf("--claude-session needs a value")
			}
		case a == "--minutes":
			v, err := value(i, a)
			if err != nil {
				return req, err
			}
			if err := minutes(v); err != nil {
				return req, err
			}
			i++
		case strings.HasPrefix(a, "--minutes="):
			if err := minutes(strings.TrimPrefix(a, "--minutes=")); err != nil {
				return req, err
			}
		case strings.HasPrefix(a, "-"):
			return req, fmt.Errorf("unknown flag %q to ansa hung", a)
		default:
			if req.To != "" {
				return req, fmt.Errorf("ansa hung takes one target, got %q and %q (use --all for every claude pane)", req.To, a)
			}
			req.To = a
		}
	}
	return req, nil
}
