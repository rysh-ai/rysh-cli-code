// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"strings"
	"testing"
)

func TestParseWarp(t *testing.T) {
	p, err := ParseWarp(readFixture(t, "warp-dev.yaml"), fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Dev" {
		t.Errorf("name = %q", p.Name)
	}
	assertShape(t, p, []string{
		"docs: [$HOME/Documents|ls -la]",
		"app: [$HOME/src/app|npm install; npm start, $HOME/src/app|npm test -- --watch, /srv/logs|]",
		"second window: [/tmp|]",
	})
	if !hasNote(p, "flattened") || !hasNote(p, "windows were merged") {
		t.Errorf("notes = %q", p.Notes)
	}
}

func TestParseWarp_SideBySide(t *testing.T) {
	p, err := ParseWarp([]byte(`
name: x
windows:
  - tabs:
      - title: t
        layout:
          split_direction: vertical
          panes:
            - cwd: /a
            - cwd: /b
`), fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	assertShape(t, p, []string{"t: [/a|] [/b|]"})
	if _, err := ParseWarp([]byte("name: empty\n"), fixtureHome); err == nil {
		t.Error("a file with no windows must be an error")
	}
}

func TestParseITerm2(t *testing.T) {
	p, err := ParseITerm2(readFixture(t, "iterm2-arrangement.plist"), fixtureHome, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Work" {
		t.Errorf("default arrangement = %q, want Work", p.Name)
	}
	assertShape(t, p, []string{
		"editor: [$HOME/src/api|vim .] [$HOME/src/api|, /srv/logs & more|]",
		"tab 2: [$HOME|htop]",
	})
	for _, tab := range p.Tabs {
		for _, l := range tab.Lanes {
			for _, pane := range l.Panes {
				if strings.Contains(pane.Command, "ignored-when-not-custom") {
					t.Error("a profile command without Custom Command: Yes must not become a start command")
				}
			}
		}
	}
}

func TestParseITerm2_PickAndErrors(t *testing.T) {
	data := readFixture(t, "iterm2-arrangement.plist")
	p, err := ParseITerm2(data, fixtureHome, "Other")
	if err != nil {
		t.Fatal(err)
	}
	assertShape(t, p, []string{"tab 1: [/tmp|]"})
	if _, err := ParseITerm2(data, fixtureHome, "Nope"); err == nil || !strings.Contains(err.Error(), "Other, Work") {
		t.Errorf("unknown arrangement must list the ones there are, got %v", err)
	}
	if _, err := ParseITerm2([]byte("bplist00\x00\x01"), fixtureHome, ""); err == nil || !strings.Contains(err.Error(), "plutil") {
		t.Errorf("a binary plist must name the plutil conversion, got %v", err)
	}
	for _, bad := range []string{"", "<plist>", "<foo/>", "<plist><dict><string>x</string></dict></plist>"} {
		if _, err := ParseITerm2([]byte(bad), fixtureHome, ""); err == nil {
			t.Errorf("ParseITerm2(%q) = nil error", bad)
		}
	}
}

func TestS3PreviewGolden(t *testing.T) {
	w, err := ParseWarp(readFixture(t, "warp-dev.yaml"), fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "warp.preview.txt", []byte(w.Text()))
	golden(t, "warp.rysh", []byte(Script(w)))
	i, err := ParseITerm2(readFixture(t, "iterm2-arrangement.plist"), fixtureHome, "")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "iterm2.preview.txt", []byte(i.Text()))
	golden(t, "iterm2.rysh", []byte(Script(i)))
}
