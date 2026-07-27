package msg

// WhatsApp UI data-transfer types and NATS messages for the desktop WhatsApp
// client view. These mirror the email_messages.go pattern: the web server turns
// WebSocket commands into NATS requests to a humanoid's inbox, and the humanoid
// replies on humanoid.<name>.whatsapp.<kind>, which the web server forwards back
// to the app as WebSocket events. Focus reuses MsgHumanoidSetFocus.

// WhatsAppMsgSummary is a brief overview of a received WhatsApp message for the
// list pane. ID is the short handle ("wa-3") used by whatsapp_read.
type WhatsAppMsgSummary struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	From      string `json:"from"`
	Name      string `json:"name"`
	Snippet   string `json:"snippet"`
	Time      string `json:"time"`
}

// WhatsAppMsgDetail is the full content of a single received WhatsApp message.
type WhatsAppMsgDetail struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	From      string `json:"from"`
	Name      string `json:"name"`
	Text      string `json:"text"`
	Time      string `json:"time"`
}

// ---- UI WhatsApp queries (web server → a humanoid's inbox) -----------------

// MsgHumanoidWhatsAppList asks a humanoid to return its recent received messages.
// The humanoid serves it from its WhatsAppAdapter store and replies on
// humanoid.<name>.whatsapp.list.
type MsgHumanoidWhatsAppList struct {
	Count int `json:"count"` // 0 ⇒ default (50), capped by the adapter store
}

// MsgHumanoidWhatsAppRead asks a humanoid for one message's full detail by its
// short ID. The reply is published on humanoid.<name>.whatsapp.detail.
type MsgHumanoidWhatsAppRead struct {
	ID string `json:"id"`
}

// ---- Results (humanoid → push subject → web server → app) ------------------

// MsgHumanoidWhatsAppListReply carries the recent-message listing back to the UI.
type MsgHumanoidWhatsAppListReply struct {
	HumanoidName string               `json:"humanoid_name"`
	Messages     []WhatsAppMsgSummary `json:"messages"`
	Err          string               `json:"err,omitempty"`
}

// MsgHumanoidWhatsAppReadReply carries one message's detail back to the UI.
type MsgHumanoidWhatsAppReadReply struct {
	HumanoidName string             `json:"humanoid_name"`
	Message      *WhatsAppMsgDetail `json:"message"`
	Err          string             `json:"err,omitempty"`
}

// MsgHumanoidWhatsAppChanged signals that a humanoid received a new WhatsApp
// message, so the UI can refresh its listing.
type MsgHumanoidWhatsAppChanged struct {
	HumanoidName string `json:"humanoid_name"`
}
