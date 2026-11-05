// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// providerKeyFamily is, for one provider, the key names provider.api_key may
// reference (and the conventional environment fallback reads), and the
// official API URLs at which the process environment may supply that key.
type providerKeyFamily struct {
	names        []string
	officialURLs []string
}

func providerKeyFamilyOf(provider string) providerKeyFamily {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return providerKeyFamily{[]string{"OPENAI_API_KEY"},
			[]string{"https://api.openai.com", "https://api.openai.com/v1"}}
	case "gemini":
		return providerKeyFamily{[]string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			[]string{"https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta/openai"}}
	case "ollama":
		return providerKeyFamily{[]string{"OLLAMA_API_KEY"},
			[]string{"http://127.0.0.1:11434", "http://127.0.0.1:11434/v1", "http://localhost:11434", "http://localhost:11434/v1"}}
	default: // "", claude, anthropic, claude-agentic, claude-cli, unknown
		return providerKeyFamily{[]string{"ANTHROPIC_API_KEY"}, []string{DefaultAnthropicAPIURL}}
	}
}

// envAllowed reports whether the process environment may supply this
// provider's key for apiURL: only while it is empty or an official URL. A
// custom URL (a proxy, a self-hosted gateway) gets its key from the secrets
// tier beside the config file, never from the environment.
func (f providerKeyFamily) envAllowed(apiURL string) bool {
	u := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if u == "" {
		return true
	}
	for _, o := range f.officialURLs {
		if strings.EqualFold(u, o) {
			return true
		}
	}
	return false
}

// resolveProviderKey resolves provider.api_key for the effective provider and
// API URL. A literal is returned unchanged. A reference must be the whole
// value and name one of the provider's own key names; it reads the secrets
// tier under ryshDir first, then the environment only when envAllowed(apiURL).
// Anything else is refused; the error names the field, never a value.
func resolveProviderKey(ryshDir, provider, apiURL, value string) (string, error) {
	v := strings.TrimSpace(value)
	if !strings.Contains(v, "${") {
		return value, nil
	}
	m := EnvRefPattern.FindStringSubmatch(v)
	if m == nil || m[0] != v {
		return "", errors.New("provider.api_key: a ${NAME} reference must be the whole value")
	}
	fam := providerKeyFamilyOf(provider)
	ok := false
	for _, n := range fam.names {
		if n == m[1] {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("provider.api_key may only reference ${%s} for this provider", strings.Join(fam.names, "} or ${"))
	}
	if s, found := LookupStoredSecretRef(ryshDir, m[1]); found && s != "" {
		return s, nil
	}
	if fam.envAllowed(apiURL) {
		return strings.TrimSpace(os.Getenv(m[1])), nil
	}
	return "", nil
}

// effectiveProvider is the provider name and API URL the session will use:
// the file's, unless RYSH_PROVIDER / RYSH_API_URL override them.
func effectiveProvider(cfg Config) (provider, apiURL string) {
	provider, apiURL = cfg.ProviderName, cfg.APIURL
	if v := env("RYSH_PROVIDER"); v != "" {
		provider = v
	}
	if v := env("RYSH_API_URL"); v != "" {
		apiURL = v
	}
	return provider, apiURL
}
