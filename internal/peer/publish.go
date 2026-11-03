// SPDX-License-Identifier: Apache-2.0

package peer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Publication is one local board the user explicitly published to the peer
// board (design 044 amendment 2026-10-20, N3). Only posts made from Since on
// are sent; All adds the cmd/ack kinds to the status kinds.
type Publication struct {
	Board string `json:"board"`
	Since int64  `json:"since"` // unix millis
	All   bool   `json:"all,omitempty"`
}

// Publications is the session's opt-in list, keyed by local board id. Empty
// is the default: nothing is published.
type Publications struct {
	Boards map[string]Publication `json:"boards"`
}

// Names is the published board ids, sorted.
func (p Publications) Names() []string {
	names := make([]string, 0, len(p.Boards))
	for name := range p.Boards {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var localBoardName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidLocalBoard reports whether name can be a local board id here: no path
// separators and no subject wildcards.
func ValidLocalBoard(name string) bool {
	return localBoardName.MatchString(name) && name != "." && name != ".."
}

func publicationsPath(root, session string) string {
	return filepath.Join(root, "peers", fmt.Sprintf("%x.publish.json", sha256.Sum256([]byte(session))))
}

// LoadPublications reads the session's publications; a missing file is none.
func LoadPublications(root, session string) (Publications, error) {
	p := Publications{Boards: map[string]Publication{}}
	data, err := os.ReadFile(publicationsPath(root, session))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(data, &p); err != nil {
		return Publications{Boards: map[string]Publication{}}, err
	}
	if p.Boards == nil {
		p.Boards = map[string]Publication{}
	}
	return p, nil
}

// SavePublications writes them atomically, private to the user.
func SavePublications(root, session string, p Publications) error {
	if p.Boards == nil {
		p.Boards = map[string]Publication{}
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(publicationsPath(root, session), data)
}
