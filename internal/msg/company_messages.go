// SPDX-License-Identifier: Apache-2.0

package msg

import "time"

// Company Connect (design 037) — the data the daemon's CompanyActor gathers
// about the AI-native company this session is bound to, and the three local
// NATS messages that move it to the TUI and the desktop app.
//
// The JSON tags on the data types match the rysh-server API field names, so
// internal/company decodes the server's replies straight into them and the
// same shape travels on the local bus. Nothing here is re-parsed from text.

// CompanyInfo names the company. SiteURL is derived (https://<fqdn>).
type CompanyInfo struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	FQDN        string `json:"fqdn"`
	Status      string `json:"status"`
	TemplateID  string `json:"template_id"`
	ServerURL   string `json:"server_url,omitempty"`
	SiteURL     string `json:"site_url,omitempty"`
}

// CompanyOrderLine is one priced basket line.
type CompanyOrderLine struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	UnitPence  int64  `json:"unit_pence"`
	TotalPence int64  `json:"total_pence"`
}

// CompanyOrder is one order placed on the company's site.
type CompanyOrder struct {
	ID            string             `json:"id"`
	Reference     string             `json:"reference"`
	CustomerName  string             `json:"customer_name"`
	CustomerPhone string             `json:"customer_phone"`
	CustomerEmail string             `json:"customer_email"`
	Fulfilment    string             `json:"fulfilment"`
	Address       string             `json:"address"`
	Notes         string             `json:"notes"`
	Items         []CompanyOrderLine `json:"items"`
	AmountPence   int64              `json:"amount_pence"`
	Currency      string             `json:"currency"`
	Status        string             `json:"status"`
	PaymentStatus string             `json:"payment_status"`
	CreatedAt     time.Time          `json:"created_at"`
}

// CompanyEnquiry is one visitor form submission that is not an order.
type CompanyEnquiry struct {
	ID        string    `json:"id"`
	Reference string    `json:"reference"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
	Email     string    `json:"email"`
	Message   string    `json:"message"`
	Fields    string    `json:"fields"`
	Status    string    `json:"status"`
	OwnerNote string    `json:"owner_note"`
	CreatedAt time.Time `json:"created_at"`
}

// CompanyEmail is the managed mailbox.
type CompanyEmail struct {
	Address         string `json:"address"`
	IncomingAddress string `json:"incoming_email_address"`
	OutgoingAddress string `json:"outgoing_email_address"`
	ReplyAddress    string `json:"reply_address"`
	State           string `json:"state"`
	LastError       string `json:"last_error,omitempty"`
}

// CompanyPhone is the business number. The server's phone reply carries many
// provider fields; the view needs these few, picked leniently on the way in.
type CompanyPhone struct {
	Number  string `json:"number"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// CompanyService is one selected toolbox service and its provisioning state.
type CompanyService struct {
	ServiceKey       string `json:"service_key"`
	State            string `json:"state"`
	StatusMessage    string `json:"status_message"`
	NextAction       string `json:"next_action,omitempty"`
	MonthlyCostMinor int64  `json:"monthly_cost_minor"`
	Currency         string `json:"currency"`
}

// CompanyChannel is one messaging channel attached to the company chatbot.
type CompanyChannel struct {
	Channel             string   `json:"channel"`
	Enabled             bool     `json:"enabled"`
	State               string   `json:"state"`
	StatusMessage       string   `json:"status_message"`
	CredentialsRequired []string `json:"credentials_required"`
}

// CompanyActivity is one live hint (an order, a payment, an enquiry, a
// provisioning step). Kind/Action match the server's activity envelope; a
// provisioning event is folded in with Kind "progress".
type CompanyActivity struct {
	Kind       string    `json:"kind"`
	Action     string    `json:"action"`
	ID         string    `json:"id"`
	Reference  string    `json:"reference"`
	Status     string    `json:"status"`
	Summary    string    `json:"summary"`
	OccurredAt time.Time `json:"occurred_at"`
}

// CompanyLink is the state of the daemon's connection to the server.
type CompanyLink struct {
	// Connected: the last REST fetch succeeded.
	Connected bool `json:"connected"`
	// Live: the NATS tail is up, so hints arrive without waiting for a poll.
	Live bool `json:"live"`
	// AuthFailed: the key was refused twice — revoked from the dashboard, or
	// the company is gone. The view says so instead of spinning.
	AuthFailed bool      `json:"auth_failed"`
	LastError  string    `json:"last_error,omitempty"`
	LastFetch  time.Time `json:"last_fetch"`
}

// CompanySnapshot is everything the company view renders.
type CompanySnapshot struct {
	Company   CompanyInfo       `json:"company"`
	Orders    []CompanyOrder    `json:"orders"`
	Enquiries []CompanyEnquiry  `json:"enquiries"`
	Email     *CompanyEmail     `json:"email,omitempty"`
	Phone     *CompanyPhone     `json:"phone,omitempty"`
	Services  []CompanyService  `json:"services"`
	Channels  []CompanyChannel  `json:"channels"`
	Activity  []CompanyActivity `json:"activity"`
	Link      CompanyLink       `json:"link"`
	FetchedAt time.Time         `json:"fetched_at"`
}

// MsgCompanySnapshot is pushed on company.snapshot whenever the follower has
// new data, and is the reply to MsgCompanyQuery on company.query.
type MsgCompanySnapshot struct {
	Snapshot CompanySnapshot `json:"snapshot"`
}

// MsgCompanyQuery asks the CompanyActor for its latest snapshot (request/reply
// on company.query). Sent by a TUI that has just attached and by ##company.
type MsgCompanyQuery struct{}

// MsgCompanyRefresh asks the CompanyActor to fetch now (fire-and-forget on
// company.refresh). The `g` key and `##company refresh`.
type MsgCompanyRefresh struct{}

// Company subjects. Session-prefixed like every other local subject.
func CompanySnapshotSubject() string { return T("company", "snapshot") }
func CompanyQuerySubject() string    { return T("company", "query") }
func CompanyRefreshSubject() string  { return T("company", "refresh") }
