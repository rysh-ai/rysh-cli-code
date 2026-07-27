package config

import (
	"fmt"
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
	if v, ok := os.LookupEnv(name); ok {
		// An env var that is set but empty counts as unresolved: an empty
		// key/token is never usable, and doctor should name it as missing.
		if v = strings.TrimSpace(v); v != "" {
			return v, true
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
// Returns the written path.
func WriteSecret(ryshDir, scope, name, value string) (string, error) {
	if !secretNameOK(name) {
		return "", fmt.Errorf("invalid secret name %q (use letters, digits and underscore; must not start with a digit)", name)
	}
	if scope == "" {
		scope = "default"
	}
	root := filepath.Join(ryshDir, "secrets")
	dir := filepath.Join(root, scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// Protective .gitignore so a project that tracks .rysh/ never commits
	// plaintext secrets (same content the ##secret command writes).
	gi := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(gi); err != nil {
		_ = os.WriteFile(gi, []byte("# rysh secrets — never commit these\n*\n!.gitignore\n"), 0o600)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("write secret file: %w", err)
	}
	return path, nil
}

// secretNamePat validates secret names — the same shape as an environment
// variable, and a safe single-segment filename (no separators, no "..").
var secretNamePat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretNameOK reports whether name is a legal secret/env identifier (and a
// safe single-segment filename).
func secretNameOK(name string) bool { return secretNamePat.MatchString(name) }
