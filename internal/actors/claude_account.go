// SPDX-License-Identifier: Apache-2.0

package actors

// claude_account.go — `##claude account`: switch every claude rysh starts to
// another subscription in one step.
//
// THE FLOW. Each subscription's long-lived token (`claude setup-token`) is a
// rysh secret named CLAUDE_<NAME>_OAUTH_TOKEN in the WORKSPACE scope.
// `##claude account use <name>` copies that secret into
// $HOME/.rysh/claude-account/token (0600, dir 0700), and exec-clean — which
// every claude launch already runs through — hands it to claude as
// CLAUDE_CODE_OAUTH_TOKEN (internal/agentenv/account.go; the env form is the
// founder's accepted risk, in the project decision log, 2026-11-01).
//
// ONE FIXED SCOPE (e20 C-1). The file is per-user, so every rysh session of
// this user on this machine switches together. The value it holds must
// therefore come from a scope that is at least that wide: the workspace scope
// (its session, persisted and config tiers) — never a tab scope, never the
// process environment. A tab secret of the same name would otherwise swap the
// token every session runs on the next time a claude launched in that tab.
// The scope is recorded beside the name, and a refresh from another workspace
// leaves the file alone.
//
// A running claude read its credentials when it started, so a switch takes
// effect at the next launch. `--restart` does that launch for every IDLE pane
// rysh can resume (launched by ##claude or fleetctl): stop by signal, then
// resume the same conversation with the standard nudge — the `##ansa kill` +
// `##ansa resume` pair, one pane at a time, the calling pane last. A busy pane
// is listed and left alone unless `--now` is given (e20 Q4): a fleet agent
// killed mid-merge or mid-suite leaves a MERGE_HEAD, a lock or a slot behind.
//
// Names only, everywhere: no output, log or argv here carries a token.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	claudeAccountSecretPrefix = "CLAUDE_"
	claudeAccountSecretSuffix = "_OAUTH_TOKEN"
	claudeAccountRestartFlag  = "--restart"
	claudeAccountNowFlag      = "--now"
)

// claudeAccountNamePattern is an account name as typed: what goes between the
// prefix and the suffix of the secret name.
var claudeAccountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)

// claudeAccountHome is the HOME the account directory lives under. A var so
// tests point it at a temp dir; "" disables the feature.
var claudeAccountHome = func() string { return os.Getenv("HOME") }

func claudeAccountDir() string { return agentenv.ClaudeAccountDir(claudeAccountHome()) }

// claudeAccountSecret maps an account name to its secret name. CODE is
// refused: CLAUDE_CODE_OAUTH_TOKEN is claude's own variable, not an account.
func claudeAccountSecret(account string) (string, bool) {
	a := strings.ToUpper(strings.TrimSpace(account))
	if !claudeAccountNamePattern.MatchString(a) || a == "CODE" {
		return "", false
	}
	return claudeAccountSecretPrefix + a + claudeAccountSecretSuffix, true
}

// claudeAccountFromSecret is the inverse: the account a secret name holds.
func claudeAccountFromSecret(secret string) (string, bool) {
	if !strings.HasPrefix(secret, claudeAccountSecretPrefix) || !strings.HasSuffix(secret, claudeAccountSecretSuffix) {
		return "", false
	}
	a := strings.TrimSuffix(strings.TrimPrefix(secret, claudeAccountSecretPrefix), claudeAccountSecretSuffix)
	if _, ok := claudeAccountSecret(a); !ok {
		return "", false
	}
	return strings.ToLower(a), true
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// claudeAccountActive is the active account's name, or "".
func claudeAccountActive() string {
	dir := claudeAccountDir()
	if dir == "" {
		return ""
	}
	if _, err := os.Lstat(agentenv.ClaudeAccountTokenFile(dir)); err != nil {
		return ""
	}
	if n := readTrimmed(agentenv.ClaudeAccountNameFile(dir)); n != "" {
		return n
	}
	return "(unnamed)"
}

// claudeAccountActiveScope is the workspace scope the active token came from.
func claudeAccountActiveScope() string {
	dir := claudeAccountDir()
	if dir == "" {
		return ""
	}
	return readTrimmed(agentenv.ClaudeAccountScopeFile(dir))
}

// writeFileAtomic writes data to path at 0600 by rename, so exec-clean never
// reads a half-written token.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claudeAccountWrite makes account (read from scope) active with token.
func claudeAccountWrite(account, scope, token string) error {
	dir := claudeAccountDir()
	if dir == "" {
		return fmt.Errorf("HOME is not set, so there is nowhere to keep the active account")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// Never let the directory reach a git repository by accident.
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o600)
	if err := writeFileAtomic(agentenv.ClaudeAccountTokenFile(dir), []byte(token+"\n")); err != nil {
		return err
	}
	if err := writeFileAtomic(agentenv.ClaudeAccountScopeFile(dir), []byte(scope+"\n")); err != nil {
		return err
	}
	return writeFileAtomic(agentenv.ClaudeAccountNameFile(dir), []byte(account+"\n"))
}

// claudeAccountClear turns the switch off: claude goes back to its own login.
func claudeAccountClear() error {
	dir := claudeAccountDir()
	if dir == "" {
		return nil
	}
	for _, p := range []string{
		agentenv.ClaudeAccountTokenFile(dir),
		agentenv.ClaudeAccountNameFile(dir),
		agentenv.ClaudeAccountScopeFile(dir),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// claudeAccountScope is the ONE scope account secrets are read from: the
// workspace scope. Never a tab scope, never the process environment (C-1).
func (w *WorkspaceActor) claudeAccountScope() string {
	scope, _ := w.secretWorkspaceScope()
	return scope
}

// claudeAccountList is every account in the workspace scope.
func (w *WorkspaceActor) claudeAccountList() []string {
	seen := map[string]bool{}
	var names []string
	for _, info := range w.secrets.List(w.claudeAccountScope()) {
		if a, ok := claudeAccountFromSecret(info.Name); ok && !seen[a] {
			seen[a] = true
			names = append(names, a)
		}
	}
	sort.Strings(names)
	return names
}

// claudeAccountToken resolves an account's token from the workspace scope
// only (session → persisted → config tiers of that scope). sessionOnly is true
// when the value exists only in the session tier, i.e. `use` is about to put
// it on disk for the first time.
func (w *WorkspaceActor) claudeAccountToken(account string) (secret, token string, sessionOnly, ok bool) {
	secret, valid := claudeAccountSecret(account)
	if !valid {
		return "", "", false, false
	}
	scope := w.claudeAccountScope()
	v, src, found := w.secrets.getScoped(scope, secret)
	v = strings.TrimSpace(v)
	if !found || v == "" {
		return secret, "", false, false
	}
	if src == secretSourceSession {
		_, persisted := w.secrets.kind.readPersisted(scope, secret)
		_, inCfg := w.secrets.cfg[scope][secret]
		sessionOnly = !persisted && !inCfg
	}
	return secret, v, sessionOnly, true
}

// claudeAccountRefresh re-copies the active account's secret before a launch,
// so a token replaced with `##secret set` is picked up without a second
// `use`. It reads only the workspace scope the account was activated from; a
// launch in another workspace leaves the file alone. It returns the active
// account name, or "" when none is active.
func (w *WorkspaceActor) claudeAccountRefresh(_ string) (account, note string) {
	account = claudeAccountActive()
	if account == "" || account == "(unnamed)" {
		return account, ""
	}
	if from := claudeAccountActiveScope(); from != w.claudeAccountScope() {
		return account, fmt.Sprintf("activated from workspace scope %q; not refreshed from this one", from)
	}
	if _, token, _, ok := w.claudeAccountToken(account); ok {
		if err := claudeAccountWrite(account, w.claudeAccountScope(), token); err != nil {
			return account, fmt.Sprintf("could not refresh the token file: %v", err)
		}
		return account, ""
	}
	secret, _ := claudeAccountSecret(account)
	return account, fmt.Sprintf("secret %s not found in the workspace scope — using the token saved when the account was activated", secret)
}

// handleClaudeAccount runs `##claude account …`.
func (w *WorkspaceActor) handleClaudeAccount(out *strings.Builder, paneID string, args []string) error {
	restart, now := false, false
	var rest []string
	for _, a := range args {
		switch strings.TrimSpace(a) {
		case claudeAccountRestartFlag:
			restart = true
		case claudeAccountNowFlag:
			now = true
		default:
			rest = append(rest, a)
		}
	}
	sub := ""
	if len(rest) > 0 {
		sub = strings.ToLower(strings.TrimSpace(rest[0]))
	}
	if now && !restart {
		fmt.Fprintf(out, "##claude account: %s goes with %s\n", claudeAccountNowFlag, claudeAccountRestartFlag)
		return fmt.Errorf("##claude account: --now without --restart")
	}
	switch sub {
	case "", "list", "ls", "status":
		if restart {
			fmt.Fprintf(out, "##claude account: %s goes with `use <name>` or `off`\n", claudeAccountRestartFlag)
			return fmt.Errorf("##claude account: --restart without use/off")
		}
		w.claudeAccountStatus(out)
		return nil
	case "use", "switch":
		if len(rest) != 2 {
			fmt.Fprintf(out, "##claude account: name one account: ##claude account use <name> [%s [%s]]\n", claudeAccountRestartFlag, claudeAccountNowFlag)
			return fmt.Errorf("##claude account use: name one account")
		}
		account := strings.ToLower(strings.TrimSpace(rest[1]))
		secret, token, sessionOnly, ok := w.claudeAccountToken(account)
		if secret == "" {
			fmt.Fprintf(out, "##claude account: %q is not a valid account name (letters, digits, _; not \"code\")\n", rest[1])
			return fmt.Errorf("##claude account: bad name %q", rest[1])
		}
		if !ok {
			fmt.Fprintf(out, "##claude account: no secret %s in this workspace — add it first:\n", secret)
			fmt.Fprintf(out, "  claude setup-token                      (logged in as that account; prints the token)\n")
			fmt.Fprintf(out, "  ##secret set %s <token>\n", secret)
			fmt.Fprintf(out, "  (tab-scoped secrets and environment variables are not used: the switch is per-user)\n")
			return fmt.Errorf("##claude account: secret %s not found", secret)
		}
		if err := claudeAccountWrite(account, w.claudeAccountScope(), token); err != nil {
			fmt.Fprintf(out, "##claude account: could not save the active account: %v\n", err)
			return fmt.Errorf("##claude account: %v", err)
		}
		fmt.Fprintf(out, "[rysh] claude account is now %q (secret %s)\n", account, secret)
		fmt.Fprintf(out, "  per-user: every rysh session of this user on this machine now starts claude on it\n")
		if sessionOnly {
			fmt.Fprintf(out, "  note: %s was a session-only secret; it is now on disk (0600) until ##claude account off\n", secret)
		}
	case "off", "none", "clear":
		if len(rest) != 1 {
			fmt.Fprintf(out, "##claude account: off takes no name: ##claude account off [%s [%s]]\n", claudeAccountRestartFlag, claudeAccountNowFlag)
			return fmt.Errorf("##claude account off: no arguments")
		}
		if err := claudeAccountClear(); err != nil {
			fmt.Fprintf(out, "##claude account: could not clear the active account: %v\n", err)
			return fmt.Errorf("##claude account: %v", err)
		}
		fmt.Fprintf(out, "[rysh] claude account switch is off — claude uses its own login again\n")
	default:
		ryshWriter(out).Unknown("claude account", sub)
		claudeAccountUsage(out)
		return fmt.Errorf("##claude account: unknown subcommand %q", sub)
	}
	if !restart {
		fmt.Fprintf(out, "  running claudes keep the account they started with; new launches and restores get the new one\n")
		fmt.Fprintf(out, "  add %s to restart idle rysh-launched claudes now (conversations are resumed)\n", claudeAccountRestartFlag)
		return nil
	}
	w.claudeAccountRestartPanes(out, paneID, now)
	return nil
}

func claudeAccountUsage(out *strings.Builder) {
	fmt.Fprintf(out, "  ##claude account                     list accounts and the active one\n")
	fmt.Fprintf(out, "  ##claude account use <name>          every claude rysh starts uses <name>\n")
	fmt.Fprintf(out, "  ##claude account off                 back to each claude's own login\n")
	fmt.Fprintf(out, "    %s  restart idle rysh-launched claudes now (resumed)\n", claudeAccountRestartFlag)
	fmt.Fprintf(out, "    %s      with %s: busy ones too (stops them mid-turn)\n", claudeAccountNowFlag, claudeAccountRestartFlag)
	fmt.Fprintf(out, "  an account is a WORKSPACE secret named CLAUDE_<NAME>_OAUTH_TOKEN holding a `claude setup-token`\n")
	fmt.Fprintf(out, "  token; the switch is per-user (all rysh sessions on this machine); claude's tools can read it\n")
}

func (w *WorkspaceActor) claudeAccountStatus(out *strings.Builder) {
	active := claudeAccountActive()
	accounts := w.claudeAccountList()
	fmt.Fprintf(out, "claude accounts (workspace secrets named CLAUDE_<NAME>_OAUTH_TOKEN):\n")
	if len(accounts) == 0 {
		fmt.Fprintf(out, "  (none yet)\n")
	}
	for _, a := range accounts {
		mark := " "
		if a == active {
			mark = "*"
		}
		secret, _ := claudeAccountSecret(a)
		fmt.Fprintf(out, "  %s %-12s %s\n", mark, a, secret)
	}
	switch active {
	case "":
		fmt.Fprintf(out, "active: none — every claude uses its own login\n")
	default:
		fmt.Fprintf(out, "active: %s — every claude rysh starts uses this account\n", active)
	}
	claudeAccountUsage(out)
}

// claudeAccountRestartable reports whether rysh launched p's claude and can
// therefore resume its conversation by id.
func claudeAccountRestartable(p ansaPane) bool {
	return claudeAccountStamped(p) && ansaPaneAgent(p) == nativeAgentClaude
}

// claudeAccountStamped reports whether rysh recorded which agent p runs.
func claudeAccountStamped(p ansaPane) bool {
	return strings.TrimSpace(p.meta(metaAgentKind)) != "" ||
		strings.TrimSpace(p.meta("fleet.agent")) != "" ||
		strings.TrimSpace(p.meta("fleet.role")) != ""
}

// claudeAccountBusy reports whether p's claude looks mid-turn. A screen that
// cannot be read counts as busy: not knowing is not a reason to kill.
func claudeAccountBusy(tr ansaTransport, paneID string) bool {
	rows, err := tr.Screen(paneID)
	if err != nil {
		return true
	}
	return ansaScreenIsWorking(rows)
}

// claudeAccountRestartPanes stops and resumes every rysh-launched claude that
// is running, the calling pane last (its own restart ends the turn that typed
// the command).
func (w *WorkspaceActor) claudeAccountRestartPanes(out *strings.Builder, callerPaneID string, now bool) {
	claudeAccountRestartVia(out, w.ansaRouterFor(), callerPaneID, now)
}

// claudeAccountRestartVia is the restart over any router (tests use a fake).
// Without now, a busy pane is listed and left running.
func claudeAccountRestartVia(out *strings.Builder, r *ansaRouter, callerPaneID string, now bool) {
	panes, err := r.tr.Panes()
	if err != nil {
		fmt.Fprintf(out, "  restart: cannot enumerate panes: %v — nothing was restarted\n", err)
		return
	}
	var targets []ansaPane
	var self *ansaPane
	handStarted, busy := 0, 0
	for i := range panes {
		p := panes[i]
		prog, perr := r.tr.Probe(p.ID)
		if perr != nil || prog != nativeAgentClaude {
			continue
		}
		if claudeAccountStamped(p) && !claudeAccountRestartable(p) {
			continue // rysh launched another agent here (codex)
		}
		if !claudeAccountRestartable(p) {
			handStarted++
			fmt.Fprintf(out, "  not restarted %s  %s — claude was started by hand; restart it yourself\n", p.ID, ansaPersona(p))
			continue
		}
		// The calling pane is busy by definition (it is running this command),
		// so it is exempt from the idle check and always goes last.
		if p.ID == callerPaneID {
			self = &panes[i]
			continue
		}
		if !now && claudeAccountBusy(r.tr, p.ID) {
			busy++
			fmt.Fprintf(out, "  busy, left running %s  %s — restart later, or add %s\n", p.ID, ansaPersona(p), claudeAccountNowFlag)
			continue
		}
		targets = append(targets, p)
	}
	if self != nil {
		targets = append(targets, *self)
	}
	restarted, failed := 0, 0
	for _, p := range targets {
		o := r.killPane(p)
		if o.Err != nil || !o.Dead {
			failed++
			reason := "not verified stopped"
			if o.Err != nil {
				reason = o.Err.Error()
			}
			fmt.Fprintf(out, "  FAILED  %s  %s — %s; left as it was\n", p.ID, ansaPersona(p), reason)
			continue
		}
		sid := nativeAgentSessionID(nativeAgentClaude, p.ID, p.meta)
		cmd, lerr := ansaResumeLine(p, nativeAgentClaude, sid)
		if lerr == nil {
			lerr = r.tr.Deliver(p.ID, msg.AnsaModeShell, cmd, "")
		}
		if lerr != nil {
			failed++
			fmt.Fprintf(out, "  FAILED  %s  %s — stopped but not resumed: %v (run ##ansa resume or ##claude in it)\n", p.ID, ansaPersona(p), lerr)
			continue
		}
		restarted++
		fmt.Fprintf(out, "  restarted %s  %s (session %s)\n", p.ID, ansaPersona(p), sid)
	}
	fmt.Fprintf(out, "%d claude panes restarted on the new account, %d busy (left running), %d failed, %d started by hand (not touched)\n",
		restarted, busy, failed, handStarted)
	if busy > 0 {
		fmt.Fprintf(out, "  busy = the screen shows a turn in progress or could not be read; a very narrow pane can hide that, so check before %s\n", claudeAccountNowFlag)
	}
	if restarted > 0 {
		fmt.Fprintf(out, "  each restarted conversation was resumed with a \"continue\" nudge\n")
	}
}
