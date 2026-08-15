// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// Channel adapters persist a little per-account state — short-ID maps and (for
// WhatsApp, which can't re-fetch) the recent-message store — so message IDs
// survive a daemon restart. State lives under .rysh/channel-state/<channel>/,
// relative to the daemon's working directory (the workspace), matching the
// .rysh convention used for secrets. It is best-effort: failures never block the
// channel.

var stateKeyUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func channelStatePath(channel, account string) string {
	safe := stateKeyUnsafe.ReplaceAllString(account, "_")
	if safe == "" {
		safe = "default"
	}
	return filepath.Join(".rysh", "channel-state", channel, safe+".json")
}

// loadChannelState reads and JSON-unmarshals persisted state into v. Returns
// false when the account is empty, the file is absent, or it cannot be parsed.
func loadChannelState(channel, account string, v interface{}) bool {
	if account == "" {
		return false
	}
	data, err := os.ReadFile(channelStatePath(channel, account))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// saveChannelState JSON-marshals v and writes it (via a temp file + rename so a
// crash can't leave a half-written file). No-op for an empty account.
func saveChannelState(channel, account string, v interface{}) error {
	if account == "" {
		return nil
	}
	path := channelStatePath(channel, account)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
