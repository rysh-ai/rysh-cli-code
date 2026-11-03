// SPDX-License-Identifier: Apache-2.0

// Package familyapply is `rysh family apply` (rysh-family H2, design 093
// §12.3): it makes a hosted family's rysh session match ONE manifest built by
// rysh-server — one native pane per avatar, the family fleet, and each
// avatar's agent profile (its system prompt and the fixed family tool
// allow-list) — idempotently, talking to the local session daemon over its
// bus the way `rysh fleet` and `rysh family serve` do.
//
// The server's hosted engine used to do this with a dozen `docker exec`s per
// build (`##pane list`, `##new pane`, `##pane name`, `fleet register|watch|member
// add`, `##pane profile set` + read back). Apply does it in one exec, touches
// only what differs, retires the panes of avatars that left, and fails unless
// the read-back shows every avatar pane holding exactly the family tools and
// its prompt (the e20 H1 P1 check, moved into the command).
package familyapply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on the manifest. The server builds it; these bound what a bad or
// hostile caller can make the command do.
const (
	MaxManifestBytes = 256 << 10
	MaxAvatars       = 8
	MaxNameRunes     = 40
	MaxPromptBytes   = 64 << 10 // the daemon's own profile prompt cap
)

// Pane meta keys apply owns.
const (
	// MetaAvatarID marks a pane as an avatar's: apply finds avatar panes by
	// it, and never touches a pane without it (bar the session's first pane,
	// which it claims while unclaimed, as the server engine always did).
	MetaAvatarID = "family.avatar_id"
	// MetaFleetAgent is the courier's routing stamp (`fleet member add
	// --agent rysh`): without it a board command is not delivered into the pane.
	MetaFleetAgent = "fleet.agent"
	// MetaRetired marks a pane apply retired but the daemon kept open (a
	// lane's last pane). It keeps the inert family-only profile, and it is
	// the one kind of profiled pane apply may claim again (e20 H2 N2).
	MetaRetired = "family.retired"
)

// RetiredPrompt is the system prompt of a retired pane kept open: no persona,
// no task, the three family tools still the only ones it has (e20 H2 N1).
const RetiredPrompt = "This pane belonged to a family member who has left the family. It is not in use. " +
	"Do not call any tool. To anything you receive, reply only: \"This family member is no longer here.\""

// Tools is every avatar's tool allow-list. It is fixed here, not taken from
// the manifest or a flag: an avatar can ask, reply and report status, and
// nothing else (no shell, no files, no web).
var Tools = []string{"family_ask", "family_reply", "family_status"}

// Avatar is one family member.
type Avatar struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Prompt string `json:"prompt"`
}

// Manifest is the whole desired state of the family session.
type Manifest struct {
	Fleet   string   `json:"fleet"`
	Avatars []Avatar `json:"avatars"`
}

var (
	fleetRE    = regexp.MustCompile(`^family-[0-9a-f]{12}$`)
	avatarIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	kindRE     = regexp.MustCompile(`^[a-z]{1,20}$`)
)

// Parse reads and validates one manifest. Input past MaxManifestBytes, an
// unknown field, trailing data or any invalid value is an error: the command
// changes nothing on a manifest it does not fully understand.
func Parse(r io.Reader) (Manifest, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	if len(b) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest larger than %d bytes", MaxManifestBytes)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return Manifest{}, errors.New("empty manifest (want one JSON object on stdin)")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Manifest{}, errors.New("manifest: trailing data after the JSON object")
	}
	return m, m.Validate()
}

// Validate checks every field.
func (m Manifest) Validate() error {
	if !fleetRE.MatchString(m.Fleet) {
		return fmt.Errorf("manifest: fleet %q: want family-<12 hex>", m.Fleet)
	}
	if len(m.Avatars) > MaxAvatars {
		return fmt.Errorf("manifest: %d avatars (max %d)", len(m.Avatars), MaxAvatars)
	}
	seen := map[string]bool{}
	for i, a := range m.Avatars {
		if !avatarIDRE.MatchString(a.ID) {
			return fmt.Errorf("manifest: avatar %d: id %q is not a lowercase uuid", i, a.ID)
		}
		if seen[a.ID] {
			return fmt.Errorf("manifest: avatar id %s appears twice", a.ID)
		}
		seen[a.ID] = true
		if err := validName(a.Name); err != nil {
			return fmt.Errorf("manifest: avatar %s: %w", a.ID, err)
		}
		if !kindRE.MatchString(a.Kind) {
			return fmt.Errorf("manifest: avatar %s: kind %q: want 1-20 letters a-z", a.ID, a.Kind)
		}
		if err := validPrompt(a.Prompt); err != nil {
			return fmt.Errorf("manifest: avatar %s: %w", a.ID, err)
		}
	}
	return nil
}

func validName(s string) error {
	if strings.TrimSpace(s) == "" || strings.TrimSpace(s) != s {
		return errors.New("name is empty or has leading/trailing space")
	}
	if !utf8.ValidString(s) {
		return errors.New("name is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(s); n > MaxNameRunes {
		return fmt.Errorf("name has %d characters (max %d)", n, MaxNameRunes)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("name has a non-printable character %U", r)
		}
	}
	return nil
}

func validPrompt(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("prompt is empty")
	}
	if len(s) > MaxPromptBytes {
		return fmt.Errorf("prompt is %d bytes (max %d)", len(s), MaxPromptBytes)
	}
	if !utf8.ValidString(s) {
		return errors.New("prompt is not valid UTF-8")
	}
	if strings.ContainsRune(s, 0) {
		return errors.New("prompt contains a NUL byte")
	}
	return nil
}

var paneNameStrip = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// paneName is the pane given-name for an avatar name: letters, digits, _ and
// -, at most 24 (the rule the server engine used), "avatar" when nothing is
// left.
func paneName(name string) string {
	n := paneNameStrip.ReplaceAllString(name, "")
	if len(n) > 24 {
		n = n[:24]
	}
	if n == "" {
		n = "avatar"
	}
	return n
}
