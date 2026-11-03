// SPDX-License-Identifier: Apache-2.0

package agentenv

// account.go — the claude account switch (`##claude account use <name>`).
//
// The founder runs every pane's claude on one subscription and, when that one
// hits its weekly limit, moves every pane to a second one. Logging in pane by
// pane does not scale, so rysh holds both accounts' long-lived tokens (from
// `claude setup-token`) as secrets, and the daemon copies the ACTIVE one into
// a single file. exec-clean reads that file and hands the token to claude.
//
// BY ENVIRONMENT, RISK ACCEPTED (e20 C-2; founder, 2026-11-01). The token is
// set as CLAUDE_CODE_OAUTH_TOKEN in the exec'd environment, so every tool
// process the agent starts can read it. The descriptor form
// (CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR) was built and tested first: claude
// 2.1.311 reads the environment token before its stored login, but the
// descriptor only when there is NO stored login, so on a machine that has one
// the descriptor is silently ignored. The founder accepted the env form (the
// project decision log, 2026-11-01). The token still never reaches argv, a typed
// line, output or a log, and an inherited descriptor variable is removed so
// the two can never disagree.
//
// WHY A FIXED PATH AND NOT A FLAG. Every claude launch already goes through
// exec-clean (TypedLine), so the file needs no new argument: lines typed by an
// older daemon, restore lines and `##ansa resume` lines all pick it up. The
// path is derived from the HOME of the environment exec-clean was given, so a
// test that passes a synthetic environment never reads the real file.
//
// No file means no change — claude uses its own login, exactly as before. A
// sandboxed launch (--sandbox) is not given the account: the sandbox keeps its
// own auth rules (its proxy mode drops this name).

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeOAuthTokenEnv is the name claude reads a subscription token from.
const ClaudeOAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// ClaudeOAuthTokenFDEnv is claude's descriptor alternative; removed whenever an
// account is active, so it cannot disagree with the environment token.
const ClaudeOAuthTokenFDEnv = "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"

// claudeAccountRel is the account directory under $HOME.
var claudeAccountRel = filepath.Join(".rysh", "claude-account")

// ClaudeAccountDir is the directory holding the active account, for home.
func ClaudeAccountDir(home string) string {
	if strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, claudeAccountRel)
}

// ClaudeAccountTokenFile is the token file inside dir.
func ClaudeAccountTokenFile(dir string) string { return filepath.Join(dir, "token") }

// ClaudeAccountNameFile records which account the token belongs to (a name,
// never a value), so `##claude account` can say which one is active.
func ClaudeAccountNameFile(dir string) string { return filepath.Join(dir, "active") }

// ClaudeAccountScopeFile records the secret scope the token was read from.
func ClaudeAccountScopeFile(dir string) string { return filepath.Join(dir, "scope") }

func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// readClaudeAccountToken returns the active token for env's HOME. ok is false
// when no account is active (no file). A file that exists but cannot be
// trusted is an error, so exec-clean can say so instead of silently using the
// pane's own login: a symlink, a file swapped between the check and the open,
// or a file anyone but the owner can read.
func readClaudeAccountToken(env []string) (token string, ok bool, err error) {
	dir := ClaudeAccountDir(envValue(env, "HOME"))
	if dir == "" {
		return "", false, nil
	}
	path := ClaudeAccountTokenFile(dir)
	lfi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !lfi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file (a symlink?); not using it — run ##claude account use <name> again", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !os.SameFile(lfi, fi) {
		return "", false, fmt.Errorf("%s changed while it was being opened; not using it", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", false, fmt.Errorf("%s is readable by others (mode %o); not using it — run ##claude account use <name> again", path, fi.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", false, err
	}
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", false, nil
	}
	return t, true, nil
}

// withClaudeAccount sets CLAUDE_CODE_OAUTH_TOKEN from the active account file,
// replacing any inherited value and dropping any inherited descriptor
// variable. On any problem it returns env unchanged (claude's own login) and
// writes one line, never a value, to stderr.
func withClaudeAccount(env []string, stderr io.Writer, explain bool) []string {
	token, ok, err := readClaudeAccountToken(env)
	if err != nil {
		fmt.Fprintf(stderr, "exec-clean: claude account: %v\n", err)
		return env
	}
	if !ok {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case ClaudeOAuthTokenEnv, ClaudeOAuthTokenFDEnv:
			continue
		}
		out = append(out, kv)
	}
	if explain {
		fmt.Fprintf(stderr, "%s (set from the active claude account)\n", ClaudeOAuthTokenEnv)
	}
	return append(out, ClaudeOAuthTokenEnv+"="+token)
}
