// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// ShortID is the first 8 characters of an id — enough to type back as a prefix.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// PctString renders a pct (0–100) as "40%", or "" when unset.
func PctString(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// StatusString is "thinking 40%" / "idle".
func StatusString(status string, pct *float64) string {
	if status == "" {
		status = "idle"
	}
	if s := PctString(pct); s != "" {
		return status + " " + s
	}
	return status
}

func sortedAvatars(av []Avatar) []Avatar {
	out := append([]Avatar(nil), av...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SeatIndex != out[j].SeatIndex {
			return out[i].SeatIndex < out[j].SeatIndex
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// OneLine folds a body onto one line for list and event output.
func OneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ↵ ")), " ")
}

// FormatFamilies writes `rysh family ls`: one block per family, one row per
// avatar with its kind and live status.
func FormatFamilies(w io.Writer, fams []Family) {
	if len(fams) == 0 {
		fmt.Fprintln(w, "no families yet (create one in the rysh-family web app)")
		return
	}
	for i, f := range fams {
		if i > 0 {
			fmt.Fprintln(w)
		}
		theme := f.Theme
		if theme == "" {
			theme = "cozy"
		}
		fmt.Fprintf(w, "%s  (%s, %s, %d avatar%s)\n", f.Name, ShortID(f.ID), theme, len(f.Avatars), plural(len(f.Avatars)))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, a := range sortedAvatars(f.Avatars) {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", a.Name, a.Kind, StatusString(a.Status, a.Pct), OneLine(a.Note))
		}
		tw.Flush()
	}
}

// FormatFamily writes `rysh family show`: the avatars with status, pct and
// note, then the family's open tasks (state open or doing).
func FormatFamily(w io.Writer, f Family, tasks []Task) {
	theme := f.Theme
	if theme == "" {
		theme = "cozy"
	}
	fmt.Fprintf(w, "%s  id %s  theme %s\n\n", f.Name, f.ID, theme)
	names := map[string]string{}
	if len(f.Avatars) == 0 {
		fmt.Fprintln(w, "no avatars")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "AVATAR\tKIND\tSTATUS\tPCT\tID\tNOTE")
		for _, a := range sortedAvatars(f.Avatars) {
			names[a.ID] = a.Name
			status := a.Status
			if status == "" {
				status = "idle"
			}
			pct := PctString(a.Pct)
			if pct == "" {
				pct = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Kind, status, pct, ShortID(a.ID), OneLine(a.Note))
		}
		tw.Flush()
	}

	var open []Task
	for _, t := range tasks {
		if t.State != "done" {
			open = append(open, t)
		}
	}
	fmt.Fprintf(w, "\nopen tasks: %d\n", len(open))
	if len(open) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, t := range open {
		who := names[t.AvatarID]
		if who == "" {
			who = ShortID(t.AvatarID)
		}
		pct := PctString(t.Pct)
		if pct == "" {
			pct = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", who, t.State, pct, OneLine(t.Title))
	}
	tw.Flush()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// EventFormatter turns socket events into one line each. It learns avatar
// names from `hello` and `avatar` frames so later frames, which carry only
// ids, print names.
type EventFormatter struct {
	names map[string]string
	// Clock formats the line's time prefix; nil prints no prefix.
	Clock func(time.Time) string
}

// NewEventFormatter returns a formatter seeded with fam's avatar names
// (fam may be the zero Family).
func NewEventFormatter(fam Family) *EventFormatter {
	f := &EventFormatter{names: map[string]string{}}
	for _, a := range fam.Avatars {
		f.names[a.ID] = a.Name
	}
	return f
}

// Name is the avatar's name, or its short id when unknown.
func (f *EventFormatter) Name(id string) string {
	if n := f.names[id]; n != "" {
		return n
	}
	if id == "" {
		return "?"
	}
	return ShortID(id)
}

// Observe updates the name table from an event without formatting it.
func (f *EventFormatter) Observe(ev Event) {
	switch ev.Type {
	case "hello":
		if ev.Family != nil {
			for _, a := range ev.Family.Avatars {
				f.names[a.ID] = a.Name
			}
		}
	case "avatar":
		if ev.Avatar != nil {
			f.names[ev.Avatar.ID] = ev.Avatar.Name
		}
	}
}

// Line formats ev as one line (no trailing newline). It returns "" for
// events that `watch` does not print (streamed `delta` chunks — the whole
// reply prints once, on `reply_done`).
func (f *EventFormatter) Line(ev Event) string {
	f.Observe(ev)
	var s string
	switch ev.Type {
	case "hello":
		if ev.Family != nil {
			s = fmt.Sprintf("hello    connected to %s (%d avatar%s)", ev.Family.Name, len(ev.Family.Avatars), plural(len(ev.Family.Avatars)))
		} else {
			s = "hello    connected"
		}
	case "status":
		s = fmt.Sprintf("status   %s → %s", f.Name(ev.AvatarID), StatusString(ev.Status, ev.Pct))
		if n := OneLine(ev.Note); n != "" {
			s += " (" + n + ")"
		}
	case "note":
		s = fmt.Sprintf("note     %s → %s: %s", f.Name(ev.FromAvatarID), f.Name(ev.ToAvatarID), OneLine(ev.Text))
	case "task":
		if ev.Task == nil {
			return ""
		}
		t := ev.Task
		st := t.State
		if p := PctString(t.Pct); p != "" {
			st += " " + p
		}
		s = fmt.Sprintf("task     %s [%s] %s", f.Name(t.AvatarID), st, OneLine(t.Title))
	case "message":
		if ev.Message == nil {
			return ""
		}
		s = "message  " + f.messageLine(*ev.Message)
	case "reply_done":
		if ev.Message == nil {
			s = fmt.Sprintf("reply    %s: (done)", f.Name(ev.AvatarID))
		} else {
			s = fmt.Sprintf("reply    %s: %s", f.Name(ev.AvatarID), OneLine(ev.Message.Body))
		}
	case "avatar":
		if ev.Avatar == nil {
			return ""
		}
		s = fmt.Sprintf("avatar   %s (%s) %s", ev.Avatar.Name, ev.Avatar.Kind, StatusString(ev.Avatar.Status, ev.Avatar.Pct))
	case "avatar_removed":
		s = fmt.Sprintf("avatar   %s removed", f.Name(ev.AvatarID))
		delete(f.names, ev.AvatarID)
	case "error":
		s = "error    " + OneLine(ev.ErrMessage)
	case "delta":
		return ""
	default:
		s = ev.Type
	}
	if f.Clock != nil && !ev.Received.IsZero() {
		s = f.Clock(ev.Received) + " " + s
	}
	return s
}

func (f *EventFormatter) messageLine(m Message) string {
	body := OneLine(m.Body)
	switch m.Direction {
	case "user":
		return fmt.Sprintf("you → %s: %s", f.Name(m.AvatarID), body)
	case "avatar":
		return fmt.Sprintf("%s: %s", f.Name(m.AvatarID), body)
	case "avatar_to_avatar":
		from, to := m.AvatarID, ""
		if m.FromAvatarID != nil {
			from = *m.FromAvatarID
		}
		if m.ToAvatarID != nil {
			to = *m.ToAvatarID
		}
		return fmt.Sprintf("%s → %s: %s", f.Name(from), f.Name(to), body)
	case "system":
		return fmt.Sprintf("system [%s]: %s", f.Name(m.AvatarID), body)
	}
	return fmt.Sprintf("%s [%s]: %s", m.Direction, f.Name(m.AvatarID), body)
}
