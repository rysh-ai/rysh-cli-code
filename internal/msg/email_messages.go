package msg

// Email data-transfer types. These live in the msg package (rather than
// internal/channels) so they can be embedded in the NATS messages below without
// an import cycle (channels imports msg, not the other way around). The
// EmailAdapter returns these types directly.

// EmailSummary holds a brief overview of an email for listing. MessageID and
// InReplyTo are carried so a UI can thread messages and identify which email to
// act on (e.g. set reply In-Reply-To) without a full ReadEmail.
type EmailSummary struct {
	ID        string `json:"id"` // short 4-char handle for prompts (stable per session)
	UID       int    `json:"uid"`
	From      string `json:"from"`
	Subject   string `json:"subject"`
	Date      string `json:"date"`
	Snippet   string `json:"snippet"`
	MessageID string `json:"message_id"`
	InReplyTo string `json:"in_reply_to"`
}

// EmailDetail holds the full content of a single email.
type EmailDetail struct {
	ID          string           `json:"id"` // short 4-char handle for prompts
	UID         int              `json:"uid"`
	From        string           `json:"from"`
	To          string           `json:"to"`
	Subject     string           `json:"subject"`
	Date        string           `json:"date"`
	Body        string           `json:"body"`
	MessageID   string           `json:"message_id"`
	InReplyTo   string           `json:"in_reply_to"`
	Attachments []AttachmentInfo `json:"attachments,omitempty"`
}

// AttachmentInfo describes an email attachment without the full data.
type AttachmentInfo struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// ---- UI email queries (web server → a humanoid's inbox) -------------------

// MsgHumanoidEmailList asks a humanoid to fetch its inbox listing. The humanoid
// serves it from its EmailAdapter and replies on humanoid.<name>.email.list.
type MsgHumanoidEmailList struct {
	Count  int    `json:"count"`  // 0 ⇒ default (10), capped at 50 by the adapter
	Search string `json:"search"` // optional SUBJECT substring filter
}

// MsgHumanoidEmailRead asks a humanoid to fetch one email's full detail. The
// reply is published on humanoid.<name>.email.detail.
type MsgHumanoidEmailRead struct {
	UID int `json:"uid"`
}

// ---- Results (humanoid → push subject → web server → app) -----------------

// MsgHumanoidEmailListReply carries an inbox listing back toward the UI.
type MsgHumanoidEmailListReply struct {
	HumanoidName string         `json:"humanoid_name"`
	Emails       []EmailSummary `json:"emails"`
	Err          string         `json:"err,omitempty"`
}

// MsgHumanoidEmailReadReply carries a single email's detail back toward the UI.
type MsgHumanoidEmailReadReply struct {
	HumanoidName string       `json:"humanoid_name"`
	Email        *EmailDetail `json:"email"`
	Err          string       `json:"err,omitempty"`
}

// MsgHumanoidEmailChanged signals that a humanoid's inbox changed (new mail
// arrived via IMAP IDLE), so the UI can refresh its listing.
type MsgHumanoidEmailChanged struct {
	HumanoidName string `json:"humanoid_name"`
}

// MsgHumanoidEmailCompose asks a humanoid to send an email the human wrote
// themselves (the manual-compose path of the email client, distinct from the
// AI-draft path). The human authored and sent it, so it needs no separate
// approval gate: the humanoid stages it as an approved draft and sends it over
// SMTP. Published to humanoid.<name>.inbox; the humanoid replies on
// humanoid.<name>.email.compose.
type MsgHumanoidEmailCompose struct {
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	InReplyTo string `json:"in_reply_to,omitempty"` // MessageID being replied to, for threading
}

// MsgHumanoidEmailComposeReply reports the outcome of a MsgHumanoidEmailCompose
// send back toward the UI.
type MsgHumanoidEmailComposeReply struct {
	HumanoidName string `json:"humanoid_name"`
	Ok           bool   `json:"ok"`
	Err          string `json:"err,omitempty"`
}

// MsgHumanoidSetFocus tells a humanoid which email the desktop UI currently has
// open (Listing=false), or that the user is browsing the inbox list with no
// email open (Listing=true). The humanoid prefers this over lastInbound when
// enriching the LLM prompt, so "reply to this" acts on the email the user is
// actually looking at — even an older one. Body is included so the LLM need not
// re-read the message; MessageID is used as the reply In-Reply-To.
type MsgHumanoidSetFocus struct {
	Listing   bool   `json:"listing"`
	UID       int    `json:"uid"`
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	From      string `json:"from"`
	Subject   string `json:"subject"`
	Body      string `json:"body,omitempty"`
}
