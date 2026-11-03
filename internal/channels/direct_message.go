// SPDX-License-Identifier: Apache-2.0

package channels

// directMessagesAddressed is the single switch behind E33 G1: a 1:1 direct
// message to the bot counts as addressed even in reply_mode: mentions —
// Telegram chat.type "private", Discord a message with no guild, Slack
// channel_type "im". Mentions-only exists to keep a bot quiet in a shared
// channel; in a DM there is nobody else to address, so treating a plain "hi"
// as observe-only meant the first message a new user sends got silence.
// Groups, channels and group DMs keep mentions-only.
//
// It changes WHAT counts as addressed, never WHO is answered: the humanoid's
// pairing gate runs before the observe-only check, so a DM from a sender who
// is not on the allowlist is still held as a pairing request.
//
// Founder question Q6 is open on whether this applies to every humanoid or
// only the assistant profile. This build applies it to every humanoid (it is
// adapter-level). Should Q6 come back "assistant only", the narrowing is in
// HumanoidActor's observe-only branch: treat direct_message=true without
// mention=true as observe-only unless isAssistantProfile().
const directMessagesAddressed = true

// tagDirectMessage marks a 1:1 DM in the inbound metadata and reports whether
// it counts as addressed under mentions-only reply mode.
func tagDirectMessage(metadata map[string]string) bool {
	metadata["direct_message"] = "true"
	return directMessagesAddressed
}
