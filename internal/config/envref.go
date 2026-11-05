// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file backs the secret-reference handling used by `rysh onboard` and
// `rysh doctor` (design 004): config and skill files carry ${NAME} references,
// never literal secrets. The literal value lives in the .rysh/secrets tier
// (written here with the same layout/permissions the ##secret command uses) or
// in the process environment.

// EnvRefPattern matches ${NAME} secret/environment references in config values.
// The shape mirrors the secret-name rules used by the ##secret command: a
// leading letter/underscore followed by letters, digits, or underscores.
var EnvRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// EnvRefNames returns the distinct ${NAME} reference names found in s, in
// order of first appearance.
func EnvRefNames(s string) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range EnvRefPattern.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	return names
}

// ExpandEnvRefs replaces every ${NAME} reference in s using lookup. References
// that lookup cannot resolve are left untouched and reported in missing (each
// name once, in order of first appearance).
func ExpandEnvRefs(s string, lookup func(string) (string, bool)) (expanded string, missing []string) {
	seen := map[string]bool{}
	expanded = EnvRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := EnvRefPattern.FindStringSubmatch(match)[1]
		if v, ok := lookup(name); ok {
			return v
		}
		if !seen[name] {
			seen[name] = true
			missing = append(missing, name)
		}
		return match
	})
	return expanded, missing
}

// LookupSecretRef resolves a ${NAME} reference through the tiers available
// outside a running session: the persisted .rysh/secrets files under ryshDir
// (the "default" scope first, then any other scope, alphabetically), then the
// process environment. The session-KV tier only exists inside a daemon, so
// this is the config-level precedence `rysh onboard` / `rysh doctor` can see.
// Values are whitespace-trimmed like the runtime secret store's.
func LookupSecretRef(ryshDir, name string) (string, bool) {
	if v, ok := LookupStoredSecretRef(ryshDir, name); ok {
		return v, true
	}
	if !secretNameOK(name) {
		return "", false
	}
	if v, ok := os.LookupEnv(name); ok {
		// An env var that is set but empty counts as unresolved: an empty
		// key/token is never usable, and doctor should name it as missing.
		if v = strings.TrimSpace(v); v != "" {
			return v, true
		}
	}
	return "", false
}

// LookupStoredSecretRef is LookupSecretRef without the process environment:
// only the persisted .rysh/secrets files. It is what an already-running
// daemon can resolve no matter which environment it was started from.
func LookupStoredSecretRef(ryshDir, name string) (string, bool) {
	if !secretNameOK(name) {
		return "", false
	}
	root := filepath.Join(ryshDir, "secrets")
	// Preferred scope first, then every other scope directory.
	if v, ok := readSecretFile(filepath.Join(root, "default", name)); ok {
		return v, true
	}
	if entries, err := os.ReadDir(root); err == nil {
		var scopes []string
		for _, e := range entries {
			if e.IsDir() && e.Name() != "default" {
				scopes = append(scopes, e.Name())
			}
		}
		sort.Strings(scopes)
		for _, scope := range scopes {
			if v, ok := readSecretFile(filepath.Join(root, scope, name)); ok {
				return v, true
			}
		}
	}
	return "", false
}

// readSecretFile reads one persisted secret file, trimming surrounding
// whitespace (internal spaces are preserved, e.g. Gmail app passwords).
func readSecretFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// WriteSecret persists a literal secret value into the .rysh/secrets tier:
// <ryshDir>/secrets/<scope>/<name> with restrictive permissions (0700 dirs,
// 0600 file) and a protective .gitignore at the secrets root, mirroring the
// ##secret command's persisted-secret layout so the runtime store resolves it.
// The file is replaced atomically (temp file + rename in the same directory),
// so a reader never sees a half-written value and a symlink planted at the
// path is replaced, not followed. Returns the written path, absolute.
func WriteSecret(ryshDir, scope, name, value string) (string, error) {
	if !secretNameOK(name) {
		return "", fmt.Errorf("invalid secret name %q (use letters, digits and underscore; must not start with a digit)", name)
	}
	if scope == "" {
		scope = "default"
	}
	if abs, err := filepath.Abs(ryshDir); err == nil {
		ryshDir = abs
	}
	root := filepath.Join(ryshDir, "secrets")
	dir := filepath.Join(root, scope)
	// Never through a symlink or into another user's directory (ledger 167
	// X2): checked before MkdirAll, which would follow a symlinked root, and
	// again after it.
	for _, d := range []string{root, dir} {
		if err := checkSecretsDir(d, true); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone: tighten both levels.
	for _, d := range []string{root, dir} {
		if err := checkSecretsDir(d, false); err != nil {
			return "", err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return "", fmt.Errorf("restrict %s: %w", d, err)
		}
	}
	// Protective .gitignore so a project that tracks .rysh/ never commits
	// plaintext secrets (same content the ##secret command writes).
	gi := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(gi); err != nil {
		_ = os.WriteFile(gi, []byte("# rysh secrets — never commit these\n*\n!.gitignore\n"), 0o600)
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*") // created 0600
	if err != nil {
		return "", fmt.Errorf("write secret file: %w", err)
	}
	_, werr := tmp.Write([]byte(value))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write secret file: %w", werr)
	}
	return path, nil
}

// configFieldRefs lists, per config field, the only secret names a ${NAME}
// reference in that field may use. A field that is not listed (a URL such as
// email.mailgun_base_url) takes no reference at all.
var configFieldRefs = map[string][]string{
	"web.username":                      {"RYSH_WEB_USERNAME"},
	"web.password":                      {"RYSH_WEB_PASSWORD"},
	"email.address":                     {"EMAIL_ADDRESS"},
	"email.username":                    {"EMAIL_USERNAME"},
	"email.password":                    {"EMAIL_PASSWORD"},
	"email.mailgun_domain":              {"MAILGUN_DOMAIN", "MAILGUN_SANDBOX_DOMAIN"},
	"email.mailgun_api_key":             {"MAILGUN_API_KEY"},
	"email.mailgun_webhook_signing_key": {"MAILGUN_WEBHOOK_SIGNING_KEY"},
}

// resolveFieldRef resolves one credential field of a config file. A value
// without "${" is a literal and is returned unchanged. Otherwise the whole
// value must be one ${NAME} whose NAME is on the field's list, and it is read
// from the persisted secrets tier under ryshDir only (any scope; never the
// process environment); unresolved, it is "". Anything else is refused, and
// the error names the field, never a value.
func resolveFieldRef(ryshDir, field, value string) (string, error) {
	v := strings.TrimSpace(value)
	if !strings.Contains(v, "${") {
		return value, nil
	}
	m := EnvRefPattern.FindStringSubmatch(v)
	if m == nil || m[0] != v {
		return "", fmt.Errorf("%s: a ${NAME} reference must be the whole value", field)
	}
	allowed := configFieldRefs[field]
	ok := false
	for _, name := range allowed {
		if name == m[1] {
			ok = true
			break
		}
	}
	if !ok {
		if len(allowed) == 0 {
			return "", fmt.Errorf("%s takes no ${NAME} reference; write the value itself", field)
		}
		return "", fmt.Errorf("%s may only reference ${%s}", field, strings.Join(allowed, "} or ${"))
	}
	s, _ := LookupStoredSecretRef(ryshDir, m[1])
	return s, nil
}

// checkSecretsDir refuses a secrets directory that is a symlink, not a
// directory, or owned by another user. missingOK accepts a path that does
// not exist yet.
func checkSecretsDir(d string, missingOK bool) error {
	info, err := os.Lstat(d)
	switch {
	case err != nil && missingOK && errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("check %s: %w", d, err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink; refusing to write a secret through it", d)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory; refusing to write a secret there", d)
	case !ownedByMe(info):
		return fmt.Errorf("%s is owned by another user; refusing to write a secret there", d)
	}
	return nil
}

// secretNamePat validates secret names — the same shape as an environment
// variable, and a safe single-segment filename (no separators, no "..").
var secretNamePat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretNameOK reports whether name is a legal secret/env identifier (and a
// safe single-segment filename).
func secretNameOK(name string) bool { return secretNamePat.MatchString(name) }
