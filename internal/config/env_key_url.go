// SPDX-License-Identifier: Apache-2.0

package config

import "strings"

// firstPartyUpstreams are the rysh-operated upstream servers for which the
// environment's RYSH_UPSTREAM_API_KEY may stand in for a config file's key:
// the same list as the desktop app's TRUSTED_CONNECT_HOSTS.
var firstPartyUpstreams = []string{"https://rysh.ai", "https://letchat.ai"}

func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
}

func isFirstPartyUpstream(u string) bool {
	for _, fp := range firstPartyUpstreams {
		if sameURL(u, fp) {
			return true
		}
	}
	return false
}

// upstreamEnvKeyAllowed reports whether RYSH_UPSTREAM_API_KEY may replace the
// config's upstream key: only where the server it goes to is not chosen by
// the file. The key travels to upstream.url and, for `rysh company`, to
// company.server, so both must be first-party or come from RYSH_UPSTREAM_URL.
func upstreamEnvKeyAllowed(cfg Config) bool {
	envURL := env("RYSH_UPSTREAM_URL")
	if envURL == "" && !isFirstPartyUpstream(cfg.Upstream.URL) {
		return false
	}
	if s := strings.TrimSpace(cfg.Company.Server); s != "" && !isFirstPartyUpstream(s) && !(envURL != "" && sameURL(s, envURL)) {
		return false
	}
	return true
}

// apiEnvKeyAllowed reports whether RYSH_API_KEY may replace the provider key:
// only for an API URL from RYSH_API_URL, or an empty / official one.
func apiEnvKeyAllowed(cfg Config) bool {
	if env("RYSH_API_URL") != "" {
		return true
	}
	provider, apiURL := effectiveProvider(cfg)
	return providerKeyFamilyOf(provider).envAllowed(apiURL)
}
