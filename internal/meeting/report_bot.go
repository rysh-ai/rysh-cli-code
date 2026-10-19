// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// The reporter bot in the registry (design 045). `report_on` starts a
// listen-only bot for a meeting — a Recall bot for Meet/Zoom/Teams, a LiveKit
// participant `agent-reporter` for rysh rooms — whose segments feed a
// reportTaker instead of the wake/turn machinery. The report is written when
// the bot's meeting ends for it: the call ends or it is removed (run),
// `report_off` or a meeting leave (close), the time limit, or the daemon
// stopping (Close).

// ReporterName is the reporter's agent name: its public-URL path and its
// LiveKit identity (agent-reporter). `##meeting add agent <id> reporter`
// means the reporter, so no ##agent of that name can join a meeting.
const ReporterName = "reporter"

// reporterBotName is the Recall bot's display name. It keeps the botName
// prefix so agent bots never hear it, and it tells people what it does.
const reporterBotName = botName + " — reporter, taking notes"

// ReporterConsent is what `--consent` confirms for a reporter.
const ReporterConsent = "participants know a rysh reporter bot joins to write a meeting report: the meeting's speech is transcribed (Recall or Deepgram), " +
	"the transcript is kept in memory only until it is summarized and is never saved, and the report (topics, previous tasks, blockers, plan, assignments, with speakers' display names) is saved in this workspace"

// ReporterTranscriptConsent is what `--transcript-consent` confirms when the
// owner turns on keeping the raw transcript (H-61). It replaces
// ReporterConsent, whose "never saved" would then be false.
const ReporterTranscriptConsent = "participants know a rysh reporter bot joins to write a meeting report and that the raw transcript of this meeting is kept: " +
	"the meeting's speech is transcribed (Recall or Deepgram) and every line, with the speaker's display name and time, is saved in this workspace in " + TranscriptFile + " beside the report, readable by the workspace owner only; " +
	"it is kept until the owner deletes it, and nothing deletes it automatically. The report (topics, previous tasks, blockers, plan, assignments, with speakers' display names) is saved there too"

// reportShutdownWait bounds how long the daemon's shutdown waits for a
// reporter to write what it has.
var reportShutdownWait = 45 * time.Second

// SetReportHook sets the function told about every report written (final,
// interim or failed). Set it before the first report_on.
func (m *Manager) SetReportHook(f func(ReportEvent)) {
	m.mu.Lock()
	m.reportHook = f
	m.mu.Unlock()
}

// reportEvent records a meeting's newest report and passes ev on.
func (m *Manager) reportEvent(ev ReportEvent) {
	m.mu.Lock()
	if mt := m.meetings[ev.MeetingID]; mt != nil && ev.Path != "" {
		mt.lastReport = ev.Path
	}
	hook := m.reportHook
	m.mu.Unlock()
	if hook != nil {
		hook(ev)
	}
}

// ReportDir is where a meeting's report is written: <rysh dir>/meetings/<id>.
func ReportDir(ryshDir, meetingID string) string {
	return filepath.Join(ryshDir, "meetings", meetingID)
}

// reportOn starts the meeting's reporter. c.ID, c.ReportDir and --consent
// are required; summarize is the session LLM. Called without m.mu.
func (m *Manager) reportOn(c Command, keys Credentials, summarize Execute) Reply {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Reply{Error: "meeting manager shut down"}
	}
	if c.ID == "" {
		r := Reply{Error: "report needs a meeting id; meetings: " + m.idsOrNone()}
		m.mu.Unlock()
		return r
	}
	mt, err := m.resolve(c.ID)
	if err != nil {
		m.mu.Unlock()
		return Reply{Error: err.Error()}
	}
	refuse := func(e string) Reply {
		r := mt.decorate(Reply{Error: e})
		m.mu.Unlock()
		return r
	}
	switch {
	case mt.busy != "":
		return refuse(busyError(mt))
	case mt.Reporter != nil:
		return refuse("meeting " + mt.ID + " already has its reporter (##meeting report " + mt.ID + " for its status)")
	case c.KeepTranscript && !c.TranscriptConsent:
		return refuse("keeping the raw transcript needs --transcript-consent: confirm " + ReporterTranscriptConsent)
	case !c.KeepTranscript && !c.Consent:
		return refuse("the reporter needs --consent: confirm " + ReporterConsent)
	case c.ReportDir == "" || !filepath.IsAbs(c.ReportDir):
		return refuse("the reporter needs its report directory")
	case summarize == nil:
		return refuse("the reporter needs the session's LLM (session executor unavailable)")
	}
	kind, target, server, platform, pane := mt.Kind, mt.Target, mt.Server, mt.Platform, mt.PaneID
	mt.busy, mt.busyWho = "starting", ReporterName
	m.mu.Unlock()

	taker, err := newReportTaker(ReportConfig{MeetingID: mt.ID, PaneID: pane, Origin: c.Origin, Platform: platform, Session: c.Session,
		Dir: c.ReportDir, Summarize: summarize, OnEvent: m.reportEvent, KeepTranscript: c.KeepTranscript})
	if err != nil {
		return m.finishReporter(mt, nil, nil, Reply{Error: err.Error()})
	}
	c.report, c.Agents, c.Agent, c.Allow, c.Approve = taker, nil, "", []string{"host"}, false
	slot := &botSlot{}
	var r Reply
	switch kind {
	case KindWeb:
		c.Action, c.URL, c.PublicURL = "join", target, agentPublicURL(c.PublicURL, ReporterName)
		r = m.join(slot, c, keys, summarize, nil)
	case KindLiveKit:
		c.Action, c.Server, c.Agents = "host", server, []string{ReporterName}
		r = m.joinServerRoom(slot, c, keys, summarize, nil, target)
	default:
		r = Reply{Error: "meeting " + mt.ID + " has an unknown kind"}
	}
	return m.finishReporter(mt, taker, slot.bot, r)
}

// finishReporter commits a reporter start (compare finishStart): the bot
// becomes mt.Reporter, or leaves at once when the meeting went away
// meanwhile. A start that made no bot discards the taker unwritten.
func (m *Manager) finishReporter(mt *Meeting, taker *reportTaker, bot *Service, r Reply) Reply {
	m.mu.Lock()
	mt.busy, mt.busyWho = "", ""
	abandon := bot != nil && (m.closed || mt.gone)
	if bot != nil && !abandon {
		mt.Reporter = bot
	}
	r = mt.decorate(r)
	if bot != nil && !abandon {
		r.Report = bot.reportStatus()
	}
	m.mu.Unlock()
	if bot == nil && taker != nil {
		taker.abort()
	}
	if abandon {
		left, ok := leaveBot(bot)
		r.State, r.Error, r.Notice = "stopped", "", "The meeting was closed while its reporter was starting; the reporter left."
		if !ok {
			r.State, r.Error = left.State, left.Error
		}
	}
	return r
}

// reportCommand runs report_off (the reporter leaves, the final report is
// written and announced through the hook), report_now (an interim report,
// waited for) and report_write (a report from the notes a stopped daemon
// left behind). Called without m.mu.
func (m *Manager) reportCommand(c Command, summarize Execute) Reply {
	if !ValidMeetingID(c.ID) {
		return Reply{Error: "report needs a meeting id (m-xxxxxx)"}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Reply{Error: "meeting manager shut down"}
	}
	mt := m.meetings[c.ID]
	if c.Action == "report_write" {
		live := mt != nil && mt.Reporter != nil
		m.mu.Unlock()
		if live {
			return Reply{ID: c.ID, Error: "meeting " + c.ID + " has a reporter listening; use ##meeting report " + c.ID + " now for an interim report"}
		}
		if c.ReportDir == "" {
			return Reply{ID: c.ID, Error: "the reporter needs its report directory"}
		}
		ev, err := RecoverReport(c.ReportDir, summarize)
		if err != nil {
			return Reply{ID: c.ID, Error: err.Error()}
		}
		m.reportEvent(ev)
		return Reply{ID: c.ID, State: "written", LastReport: ev.Path, Notice: "Report recovered from the reporter's notes: " + ev.Summary}
	}
	if mt == nil {
		r := Reply{Error: fmt.Sprintf("no meeting %s; meetings: %s", c.ID, m.idsOrNone())}
		m.mu.Unlock()
		return r
	}
	if mt.Reporter == nil {
		r := mt.withReport(mt.decorate(Reply{Error: "meeting " + mt.ID + " has no reporter; start one with ##meeting report " + mt.ID + " on --consent"}))
		m.mu.Unlock()
		return r
	}
	if c.Action == "report_now" {
		s := mt.Reporter
		snap := mt.copy()
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		ev := s.report.interimReport(ctx)
		cancel()
		if ev.Path != "" {
			m.mu.Lock()
			mt.lastReport = ev.Path
			m.mu.Unlock()
		}
		r := snap.decorate(Reply{State: "interim", LastReport: ev.Path, Report: s.reportStatus()})
		if ev.Error != "" {
			r.Error = ev.Error
		} else {
			r.Notice = "Interim report written: " + ev.Summary
		}
		return r
	}
	// report_off
	if mt.busy != "" {
		r := mt.decorate(Reply{Error: busyError(mt)})
		m.mu.Unlock()
		return r
	}
	s := mt.Reporter
	left, ok := m.leaveBots(mt, []*Service{s})
	snap := mt.copy()
	m.mu.Unlock()
	if !ok {
		return snap.decorate(left)
	}
	return snap.decorate(Reply{State: "stopped", Report: s.reportStatus(), Notice: "The reporter left; the report is being written and will be announced here."})
}

// reportStatus is the reporter part of a status reply.
func (s *Service) reportStatus() *ReportStatus {
	if s == nil || s.report == nil {
		return nil
	}
	st := s.report.status()
	s.mu.Lock()
	st.BotState = s.state
	s.mu.Unlock()
	return &st
}

// waitReport waits (at most d) for a reporter's final report.
func (s *Service) waitReport(d time.Duration) {
	if s.report != nil {
		s.report.wait(d)
	}
}

// abort stops a taker whose bot never started; nothing is written — not
// even the checkpoint of a chunk summary still at the LLM (cpMu).
func (r *reportTaker) abort() {
	r.finishMu.Do(func() {
		r.mu.Lock()
		r.stopped = true
		r.mu.Unlock()
		r.closeTranscript()
		r.cpMu.Lock()
		r.aborted = true
		r.cpMu.Unlock()
		close(r.quit)
	})
}
