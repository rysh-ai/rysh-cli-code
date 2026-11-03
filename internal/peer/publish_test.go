// SPDX-License-Identifier: Apache-2.0

package peer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationsRoundTripBesideTheConnection(t *testing.T) {
	root := t.TempDir()
	if got, err := LoadPublications(root, "dev-a"); err != nil || len(got.Boards) != 0 {
		t.Fatalf("fresh publications: %+v %v", got, err)
	}
	want := Publications{Boards: map[string]Publication{"default": {Board: "default", Since: 42}, "epic-07": {Board: "epic-07", Since: 43, All: true}}}
	if err := SavePublications(root, "dev-a", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPublications(root, "dev-a")
	if err != nil || len(got.Boards) != 2 || got.Boards["epic-07"] != want.Boards["epic-07"] {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if other, _ := LoadPublications(root, "dev-b"); len(other.Boards) != 0 {
		t.Fatal("another session sees this session's publications")
	}
	files, _ := filepath.Glob(filepath.Join(root, "peers", "*"))
	if len(files) != 1 {
		t.Fatalf("files under peers/: %v", files)
	}
	if info, _ := os.Stat(files[0]); info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("publications file is readable by others: %v", info.Mode())
	}
}
