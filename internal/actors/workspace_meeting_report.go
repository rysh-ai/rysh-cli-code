// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/meetingexec"
)

// `##meeting report` (design 045): a silent reporter bot that hears a whole
// meeting and writes <rysh dir>/meetings/<id>/report.md when it ends — topics,
// previous tasks and goals, blockers, the new plan and assignments.
// `##meeting add agent <id> reporter` and `remove agent <id> reporter` are
// the same as `report <id> on` and `report <id> off`.

const meetingReportUsage = "##meeting report <id> [status] | on (--consent | --keep-transcript --transcript-consent) [--public-url <https url>] | off | now | write"

// Pane meta keys for the reporter (design 045 §Pane contract).
const (
	paneMetaMeetingReporter = "meeting.reporter" // reporter bot phase: joining, in_call, left, failed ("" = none)
	paneMetaMeetingReport   = "meeting.report"   // path of the newest report.md
)

// meetingReportExec is the reporter's LLM: the session provider with no
// tools (seam for tests).
var meetingReportExec = func(w *WorkspaceActor) (meeting.Execute, error) {
	if w.agSetup == nil {
		return nil, nil // Manager answers "session executor unavailable"
	}
	return meetingexec.NewSummarizer(w.actorSystem, w.agSetup.Provider)
}

type meetingReportArgs struct {
	id, verb         string
	consent          bool
	publicURL        string
	publicURLFromCmd bool
	// H-61: keep the raw transcript for this meeting, with its own consent.
	keepTranscript, transcriptConsent bool
}

func parseMeetingReportArgs(args []string) (meetingReportArgs, error) {
	usage := fmt.Errorf("usage: %s", meetingReportUsage)
	var a meetingReportArgs
	if len(args) == 0 {
		return a, usage
	}
	a.id, a.verb = args[0], "status"
	if !meeting.ValidMeetingID(a.id) {
		return a, fmt.Errorf("%q is not a meeting id (m-xxxxxx); see ##meeting list", a.id)
	}
	rest := args[1:]
	if len(rest) > 0 {
		a.verb, rest = rest[0], rest[1:]
	}
	switch a.verb {
	case "status", "off", "now", "write":
		if len(rest) > 0 {
			return a, usage
		}
	case "on":
		for i := 0; i < len(rest); i++ {
			switch {
			case rest[i] == "--consent":
				a.consent = true
			case rest[i] == "--keep-transcript":
				a.keepTranscript = true
			case rest[i] == "--transcript-consent":
				a.transcriptConsent = true
			case rest[i] == "--public-url" && i+1 < len(rest):
				a.publicURL, a.publicURLFromCmd = rest[i+1], true
				i++
			case strings.HasPrefix(rest[i], "--public-url="):
				a.publicURL, a.publicURLFromCmd = strings.TrimPrefix(rest[i], "--public-url="), true
			default:
				return a, usage
			}
		}
	default:
		return a, usage
	}
	if a.transcriptConsent && !a.keepTranscript {
		return a, fmt.Errorf("--transcript-consent goes with --keep-transcript; without it no transcript is kept and --consent is the reporter's consent")
	}
	return a, nil
}

// meetingReportCommand handles `##meeting report …`; args start after
// "report". Provider and LLM calls run off the mailbox; results land in
// paneID.
func (w *WorkspaceActor) meetingReportCommand(out *strings.Builder, paneID string, args []string) {
	a, err := parseMeetingReportArgs(args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage: ") {
			ryshWriter(out).UsageLineIn("meeting", meetingReportUsage)
			w.failRyshUsage("%v", err)
			return
		}
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	w.meetingReport(out, paneID, a)
}

func (w *WorkspaceActor) meetingReport(out *strings.Builder, paneID string, a meetingReportArgs) {
	fail := func(err error) {
		fmt.Fprintf(out, "\n[meeting] %v\n", err)
		w.failRysh("%v", err)
	}
	dir := ""
	if w.cfg.RyshDir != "" {
		dir = meeting.ReportDir(w.cfg.RyshDir, a.id)
	}
	manager, fx := w.meetingManager(), currentMeetingEffects()
	manager.SetReportHook(func(ev meeting.ReportEvent) { w.meetingReportDone(fx, ev) })
	var mt meeting.Meeting
	known := false
	if a.verb != "write" {
		if mt, known = manager.Lookup(a.id); !known {
			fail(fmt.Errorf("no meeting %s; see ##meeting list", a.id))
			return
		}
	}
	var exec meeting.Execute
	if a.verb == "on" || a.verb == "now" || a.verb == "write" {
		var err error
		if exec, err = meetingReportExec(w); err != nil {
			fail(err)
			return
		}
	}
	switch a.verb {
	case "status":
		go func() {
			r := manager.Do(meeting.Command{Action: "status", ID: a.id}, meeting.Credentials{}, nil)
			fx.notice(w, paneID, formatReportReply(a.id, r))
		}()
	case "off":
		fmt.Fprintf(out, "\n[meeting] %s: the reporter is leaving …\n", a.id)
		go func() {
			r := manager.Do(meeting.Command{Action: "report_off", ID: a.id}, meeting.Credentials{}, nil)
			if r.Error == "" && r.PaneID != "" {
				fx.setMeta(w, r.PaneID, paneMetaMeetingReporter, "left")
			}
			fx.notice(w, paneID, formatReportReply(a.id, r))
		}()
	case "now":
		fmt.Fprintf(out, "\n[meeting] %s: writing an interim report …\n", a.id)
		go func() {
			r := manager.Do(meeting.Command{Action: "report_now", ID: a.id}, meeting.Credentials{}, exec)
			if r.Error == "" && r.PaneID != "" && r.LastReport != "" {
				fx.setMeta(w, r.PaneID, paneMetaMeetingReport, r.LastReport)
			}
			fx.notice(w, paneID, formatReportReply(a.id, r))
		}()
	case "write":
		if dir == "" {
			fail(fmt.Errorf("this session has no rysh directory for reports"))
			return
		}
		fmt.Fprintf(out, "\n[meeting] %s: writing the report from the reporter's saved notes …\n", a.id)
		go func() {
			r := manager.Do(meeting.Command{Action: "report_write", ID: a.id, ReportDir: dir}, meeting.Credentials{}, exec)
			fx.notice(w, paneID, formatReportReply(a.id, r))
		}()
	case "on":
		if dir == "" {
			fail(fmt.Errorf("this session has no rysh directory for reports"))
			return
		}
		c, keys := w.meetingReporterStart(paneID, mt, a)
		c.ReportDir = dir
		fmt.Fprintf(out, "\n[meeting] %s: starting the reporter (silent; it writes %s when the meeting ends) …\n", a.id, filepath.Join(dir, "report.md"))
		if a.keepTranscript {
			fmt.Fprintf(out, "[meeting] %s: the raw transcript is kept in %s until you delete it\n", a.id, filepath.Join(dir, meeting.TranscriptFile))
		}
		go func() {
			r := manager.Do(c, keys, exec)
			if r.PaneID != "" && r.Report != nil {
				phase := meetingBotPhase(meeting.Reply{State: r.Report.BotState})
				fx.setMeta(w, r.PaneID, paneMetaMeetingReporter, phase)
				if phase == "joining" {
					go w.watchMeetingReporter(manager, a.id, r.PaneID, fx)
				}
			}
			fx.notice(w, paneID, formatReportReply(a.id, r))
		}()
	}
}

// meetingReporterStart resolves what a reporter bot needs, as add agent
// does for an agent bot, minus the speech key: a reporter never speaks.
func (w *WorkspaceActor) meetingReporterStart(paneID string, mt meeting.Meeting, a meetingReportArgs) (meeting.Command, meeting.Credentials) {
	c := meeting.Command{Action: "report_on", ID: a.id, Consent: a.consent, KeepTranscript: a.keepTranscript, TranscriptConsent: a.transcriptConsent,
		PublicURL: a.publicURL, Origin: paneID, Session: w.cfg.SessionName}
	w.meetingSpeechVars(&c, paneID) // MEETING_STT: how a rysh-room reporter hears
	c.Defaults()
	var keys meeting.Credentials
	chain := w.secretScopeChain(paneID)
	secret := func(name string) string {
		if w.secrets == nil {
			return ""
		}
		v, _, _, _ := w.secrets.GetLayered(chain, name)
		return v
	}
	switch mt.Kind {
	case meeting.KindWeb:
		keys.Recall, keys.Verification = secret("RECALL_API_KEY"), secret("RECALL_VERIFICATION_SECRET")
		if w.variables != nil {
			if !a.publicURLFromCmd {
				c.PublicURL, _, _, _ = w.variables.GetLayered(chain, MeetingPublicURLVar)
			}
			if region, _, _, _ := w.variables.GetLayered(chain, MeetingRecallRegionVar); strings.TrimSpace(region) != "" {
				c.Region = strings.TrimSpace(region)
			}
			c.Defaults()
		}
	case meeting.KindLiveKit:
		keys.LiveKit.STTKey = secret(meeting.STTKeyName(c.STT))
		keys.RyshAPIKey = secret(meeting.RyshAPIKeySecret)
		if keys.RyshAPIKey == "" {
			keys.RyshAPIKey = w.cfg.Upstream.APIKey
		}
	}
	return c, keys
}

// watchMeetingReporter mirrors a joining reporter's phase into the pane meta
// until it is in the call, failed or gone.
func (w *WorkspaceActor) watchMeetingReporter(manager *meeting.Manager, id, paneID string, fx meetingEffects) {
	for range int(meetingBotWatchFor / fx.every) {
		time.Sleep(fx.every)
		r := manager.Do(meeting.Command{Action: "status", ID: id}, meeting.Credentials{}, nil)
		if r.PaneID != paneID || r.Report == nil {
			return
		}
		switch phase := meetingBotPhase(meeting.Reply{State: r.Report.BotState}); phase {
		case "joining":
		default:
			fx.setMeta(w, paneID, paneMetaMeetingReporter, phase)
			return
		}
	}
}

// meetingReportDone announces a report the reporter wrote (the meeting
// ended, the reporter left, the daemon stopped) in the pane that started it,
// and records its path in the meeting pane's meta.
func (w *WorkspaceActor) meetingReportDone(fx meetingEffects, ev meeting.ReportEvent) {
	if ev.PaneID != "" && ev.Path != "" {
		fx.setMeta(w, ev.PaneID, paneMetaMeetingReport, ev.Path)
	}
	if ev.PaneID != "" && !ev.Interim {
		fx.setMeta(w, ev.PaneID, paneMetaMeetingReporter, "left")
	}
	if ev.Origin == "" {
		return
	}
	text := fmt.Sprintf("\n[meeting] %s: report written: %s\n[meeting] %s\n", ev.MeetingID, ev.Path, ev.Summary)
	if ev.Error != "" {
		text = fmt.Sprintf("\n[meeting] %s: the report could not be written: %s\n", ev.MeetingID, ev.Error)
	}
	fx.notice(w, ev.Origin, text)
}

// formatReportReply renders a reporter command's result.
func formatReportReply(id string, r meeting.Reply) string {
	var b strings.Builder
	b.WriteString("\n")
	if r.Error != "" {
		fmt.Fprintf(&b, "[meeting] %s: %s\n", id, r.Error)
		return b.String()
	}
	if line := reportLine(r); line != "" {
		fmt.Fprintf(&b, "[meeting] %s: %s\n", id, line)
	} else {
		fmt.Fprintf(&b, "[meeting] %s: no reporter; start one with ##meeting report %s on --consent\n", id, id)
	}
	if r.LastReport != "" {
		fmt.Fprintf(&b, "[meeting] report: %s\n", r.LastReport)
	}
	if r.Notice != "" {
		fmt.Fprintf(&b, "[meeting] %s\n", r.Notice)
	}
	return b.String()
}

// reportLine is the one-line reporter status ("" without a reporter).
func reportLine(r meeting.Reply) string {
	if r.Report == nil {
		return ""
	}
	rs := r.Report
	line := fmt.Sprintf("reporter %s (bot %s), %d segments heard, %d parts summarized", rs.State, meetingBotPhase(meeting.Reply{State: rs.BotState}), rs.Segments, rs.Chunks)
	if len(rs.Participants) > 0 {
		line += ", speakers " + strings.Join(rs.Participants, ", ")
	}
	if rs.Transcript != "" {
		line += "; transcript kept: " + rs.Transcript
	}
	if rs.Error != "" {
		line += "; last error: " + rs.Error
	}
	return line
}
