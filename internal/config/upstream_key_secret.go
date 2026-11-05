// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The company connect key (design 037) is a credential, so it lives in the
// .rysh/secrets tier like every other secret (design 004 G4), never as a
// plain value in rysh.config.yaml (sprint 0.2.15 CD2). The YAML carries only
// the ${RYSH_UPSTREAM_API_KEY} reference, which the loader resolves. The name
// is also the long-standing environment override for upstream.api_key, so
// the two paths agree on what the key is called.
const UpstreamKeySecretName = "RYSH_UPSTREAM_API_KEY"

// UpstreamKeyRef is the reference written into upstream.api_key.
func UpstreamKeyRef() string { return "${" + UpstreamKeySecretName + "}" }

// ResolveUpstreamKey turns the upstream.api_key value of a config beside
// ryshDir into the key to send. A literal is returned unchanged. The one
// reference allowed is the WHOLE value ${RYSH_UPSTREAM_API_KEY}: it resolves
// from <ryshDir>/secrets/default only, then from the environment variable of
// that one name, else to "" (no key). Any other ${...} in the field is
// refused and nothing it names is read: the field is sent to whatever server
// the same file names, so a crafted config must not be able to pick which
// secret goes there (security-lead ledger 167 X1). The error names the field,
// never a value.
func ResolveUpstreamKey(ryshDir, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == UpstreamKeyRef() {
		if s, ok := readSecretFile(filepath.Join(ryshDir, "secrets", "default", UpstreamKeySecretName)); ok && s != "" {
			return s, nil
		}
		return strings.TrimSpace(os.Getenv(UpstreamKeySecretName)), nil
	}
	if strings.Contains(v, "${") {
		return "", errors.New("upstream.api_key: the only reference allowed is the whole value " + UpstreamKeyRef())
	}
	return value, nil
}

// StoreUpstreamKey writes key to <ryshDir>/secrets/default/RYSH_UPSTREAM_API_KEY
// (owner-only, replaced atomically) and returns the reference to put in the
// YAML and the absolute path of the file it wrote.
func StoreUpstreamKey(ryshDir, key string) (ref, path string, err error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", errors.New("the upstream key is empty")
	}
	path, err = WriteSecret(ryshDir, "default", UpstreamKeySecretName, key)
	if err != nil {
		return "", "", err
	}
	return UpstreamKeyRef(), path, nil
}

// WriteCompanyBinding writes a company binding into the config at cfgPath the
// way `rysh connect` and `##config load` both need it: the key into the
// secrets tier under ryshDir, the ${RYSH_UPSTREAM_API_KEY} reference plus the
// rest of the upstream connection and the company block into the YAML. A
// file that still holds a plain key (an older connect, or a dashboard
// download loaded as the session's own config) is thereby migrated in place.
// Returns the path of the secrets file.
func WriteCompanyBinding(cfgPath, ryshDir string, b *CompanyBinding) (string, error) {
	ref, secretsPath, err := StoreUpstreamKey(ryshDir, b.APIKey)
	if err != nil {
		return "", err
	}
	if err := SetUpstreamConnection(cfgPath, b.URL, ref, b.WorkspaceID); err != nil {
		return "", err
	}
	if err := SetCompanyBlock(cfgPath, b.Company); err != nil {
		return "", err
	}
	// The file names a credential and the company it opens: owner-only.
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		return "", err
	}
	return secretsPath, nil
}

// RyshDirForConfigFile is the rysh dir the loader pairs with an explicit
// config file: <dir>/.rysh, unless RYSH_DIR overrides it. A secret written
// for that file must go where LoadFrom(file) will look for it.
func RyshDirForConfigFile(configFile string) string {
	if v := env("RYSH_DIR"); v != "" {
		return absOrEmpty(expandHome(v))
	}
	return ryshDirForConfig(configFile)
}
