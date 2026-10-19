// SPDX-License-Identifier: Apache-2.0

package meeting

import "strings"

// Per-platform wording for what a Recall bot waits on, and how its transcript
// speakers are told apart (design 043 §"Zoom and Teams in panes"). The join
// itself is the same for every platform (createPayload); the host's steps and
// the speaker labels are not.

// recallWaiting says, while a bot is joining, which host action it waits for.
// "" for a status code that needs nothing from the host.
func recallWaiting(platform, code string) string {
	switch code {
	case "in_waiting_room":
		switch platform {
		case PlatformZoom:
			return "Bot is in the Zoom waiting room; the host must admit it (Participants → Waiting room → Admit)"
		case PlatformTeams:
			return "Bot is in the Teams lobby; the organizer must admit it (People → Waiting in lobby → Admit; its name ends in \"(Unverified)\" on teams.microsoft.com)"
		}
		return "Bot is in the waiting room/lobby; the host must admit it"
	case "in_call_not_recording", "recording_permission_allowed":
		if platform == PlatformZoom {
			return "Bot is in the call but not transcribing yet; the Zoom host must allow its recording request (the pop-up \"… is requesting permission to record\" → Allow recording)"
		}
		return "Bot is in the call but not transcribing yet"
	case "recording_permission_denied":
		return "Host denied the bot's recording request; nothing can be heard. Leave, or ask the host to allow recording"
	}
	return ""
}

// isBotSpeaker reports a transcript speaker that is a Recall bot of ours: its
// display name starts with botName. Teams appends " (Unverified)" to
// anonymous bots and platforms may re-case or pad names, so the match is on a
// trimmed, case-folded prefix. Every rysh agent bot drops the others' voices
// this way (D9), so two bots never answer each other.
func isBotSpeaker(name string) bool {
	name = strings.TrimSpace(name)
	return len(name) >= len(botName) && strings.EqualFold(name[:len(botName)], botName)
}

// unidentifiedSpeaker reports a speaker label that names nobody. Teams sends
// no identity for a participant whose "Automatically identify me in meeting
// captions and transcripts" setting is off (Recall labels it Unknown).
func unidentifiedSpeaker(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "" || n == "unknown" || n == "unknown speaker"
}

// ignoredWakeHint adds the platform reason to an ignored wake, when there is
// one the operator can act on.
func ignoredWakeHint(platform string, u Segment) string {
	if platform == PlatformTeams && u.Identity == "" && !u.Host && unidentifiedSpeaker(u.Name) {
		return "; Teams sent no speaker name (the speaker's Settings → Accessibility → \"identify me in meeting captions and transcripts\" is off): turn it on, or add the agent with --allow anyone"
	}
	return ""
}
