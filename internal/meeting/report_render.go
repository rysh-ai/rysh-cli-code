// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"fmt"
	"strings"
	"time"
)

// reportMeta is what the markdown says about how the report came to be.
type reportMeta struct {
	ended        time.Time
	final        bool
	recovered    bool   // written from notes.json after a daemon restart
	reason       string // why the meeting ended for the reporter
	note         string // a degraded final pass
	unsummarized bool   // interim: the newest part is not in the notes yet
	// transcriptErr: keeping the transcript (H-61) failed part way.
	transcriptErr string
}

// renderReport renders the report markdown (design 045 §Report). Sections
// are always present; an empty one says "None mentioned." so a reader can
// tell "not discussed" from "section missing".
func renderReport(cp reportCheckpoint, n ReportNotes, m reportMeta) string {
	var b strings.Builder
	title := "Meeting report"
	switch {
	case m.recovered:
		title += " (recovered)"
	case !m.final:
		title += " (interim — meeting in progress)"
	}
	fmt.Fprintf(&b, "# %s — %s\n\n", title, cp.MeetingID)
	platform := cp.Platform
	if platform == "" {
		platform = "meeting"
	}
	fmt.Fprintf(&b, "- **Meeting:** %s · `%s`", platformLabel(platform), cp.MeetingID)
	if cp.Session != "" {
		fmt.Fprintf(&b, " · session `%s`", cp.Session)
	}
	b.WriteString("\n")
	if !cp.Started.IsZero() {
		fmt.Fprintf(&b, "- **Reporter listened:** %s", cp.Started.Local().Format("2006-01-02 15:04"))
		if m.recovered {
			fmt.Fprintf(&b, " until at least %s into the meeting", clock(cp.Covered))
		} else if !m.ended.IsZero() {
			fmt.Fprintf(&b, " – %s (%s)", m.ended.Local().Format("15:04"), clock(m.ended.Sub(cp.Started)))
		}
		b.WriteString("\n")
	}
	if len(cp.Participants) > 0 {
		fmt.Fprintf(&b, "- **Speakers heard:** %s\n", strings.Join(cp.Participants, ", "))
	}
	if m.reason != "" {
		fmt.Fprintf(&b, "- **Ended:** %s\n", m.reason)
	}
	b.WriteString("\n## Summary\n\n")
	if n.Overview != "" {
		b.WriteString(n.Overview + "\n")
	} else if len(cp.Chunks) == 0 {
		b.WriteString("Nothing was summarized: the reporter heard no speech, or the meeting ended before the first summary.\n")
	} else {
		b.WriteString("No overview.\n")
	}

	b.WriteString("\n## Topics discussed\n\n")
	if len(n.Topics) == 0 {
		b.WriteString(none)
	}
	for _, t := range n.Topics {
		if t.Summary != "" {
			fmt.Fprintf(&b, "- **%s** — %s\n", t.Title, t.Summary)
		} else {
			fmt.Fprintf(&b, "- **%s**\n", t.Title)
		}
	}

	b.WriteString("\n## Previous tasks and goals\n\n")
	if len(n.PreviousItems) == 0 {
		b.WriteString(none)
	} else {
		b.WriteString("| Owner | Task or goal | Status (as reported) |\n|---|---|---|\n")
		for _, x := range n.PreviousItems {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(x.Owner), cell(x.Item), cell(x.Status))
		}
	}

	b.WriteString("\n## Blockers\n\n")
	if len(n.Blockers) == 0 {
		b.WriteString(none)
	}
	for _, x := range n.Blockers {
		if x.Owner != "" {
			fmt.Fprintf(&b, "- **%s:** %s\n", x.Owner, x.Item)
		} else {
			fmt.Fprintf(&b, "- %s\n", x.Item)
		}
	}

	b.WriteString("\n## New plan\n\n")
	if len(n.Plan) == 0 {
		b.WriteString(none)
	}
	for i, p := range n.Plan {
		fmt.Fprintf(&b, "%d. %s\n", i+1, p)
	}

	b.WriteString("\n## Assignments\n\n")
	if len(n.Assignments) == 0 {
		b.WriteString(none)
	} else {
		b.WriteString("| Who | What | When |\n|---|---|---|\n")
		for _, x := range n.Assignments {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(x.Who), cell(x.What), cell(x.When))
		}
	}

	if len(n.Decisions) > 0 {
		b.WriteString("\n## Decisions\n\n")
		for _, d := range n.Decisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}

	b.WriteString("\n---\n\n")
	kept := transcriptNotKeptFooter
	if cp.KeepTranscript {
		kept = transcriptKeptFooter
	}
	b.WriteString("*Written by the rysh meeting reporter from live speech-to-text with an LLM. Speaker names are the display names people joined with, not verified identities; recognition and summary errors are possible. " + kept + "*\n")
	var notes []string
	if m.transcriptErr != "" {
		notes = append(notes, "Keeping the raw transcript failed, so "+TranscriptFile+" is incomplete: "+m.transcriptErr)
	}
	if m.note != "" {
		notes = append(notes, m.note)
	}
	for _, g := range cp.Gaps {
		notes = append(notes, "Gap: "+g)
	}
	if m.unsummarized {
		notes = append(notes, "The most recent minutes are not in this interim report yet.")
	}
	if m.recovered {
		notes = append(notes, "The daemon stopped before the meeting ended; this report was written from the notes saved until then, and what was said after them is not included.")
	}
	for _, s := range notes {
		fmt.Fprintf(&b, "\n> %s\n", s)
	}
	return b.String()
}

const none = "None mentioned.\n"

// The footer's transcript sentence: the default, and the H-61 opt-in.
const (
	transcriptNotKeptFooter = "The transcript was held in memory only and was not saved."
	transcriptKeptFooter    = "The owner turned on keeping the raw transcript for this meeting: it is in `" + TranscriptFile + "` next to this report, readable by the workspace owner only, and is kept until the owner deletes it; nothing deletes it automatically."
)

// cell makes text safe inside a markdown table cell.
func cell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

func platformLabel(p string) string {
	switch p {
	case PlatformMeet:
		return "Google Meet"
	case PlatformZoom:
		return "Zoom"
	case PlatformTeams:
		return "Microsoft Teams"
	case PlatformLiveKit:
		return "rysh room (LiveKit)"
	}
	return p
}
