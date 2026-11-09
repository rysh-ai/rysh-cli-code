// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
)

// The company-write MCP server (CD5, sprint 0.2.16). Named writes a connect
// key may make once the owner ticks them for the device under "Connected
// devices" (default none). It is a separate server from the read-only
// "company" one so the session registers it with --approve and the policy
// always gates its tools: every write stops for a human (security-lead C-C2;
// enquiry text is customer-written). Each tool reads the key's own permissions
// first and, when the tick is missing or expired, sends nothing and names it.

type companyWriter interface {
	Permissions(ctx context.Context) (company.Permissions, error)
	UpdateEnquiry(ctx context.Context, enquiryID, status string, ownerNote *string) (json.RawMessage, error)
	SetOrderStatus(ctx context.Context, orderID, status string) (json.RawMessage, error)
}

// companyWriteAction names, per tool, the permission it needs and web's tick
// label for it ("Connected devices", the company's Overview).
var companyWriteAction = map[string]struct{ permission, label, verb string }{
	"company_enquiry_update": {"enquiry.update", "Update enquiry status and notes", "update enquiries"},
	"company_order_status":   {"order.status", "Change order status", "change order status"},
}

var companyWriteToolSpecs = []companyToolSpec{
	{
		Name:        "company_enquiry_update",
		Description: "Set an enquiry's status and, optionally, the owner's note on it. Nothing is sent to the customer. Needs the owner's 'Update enquiry status and notes' tick for this device; every call is approved by a human first.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"enquiry_id":{"type":"string","description":"the enquiry id (UUID) from company_enquiries"},"status":{"type":"string","enum":["new","contacted","confirmed","closed","declined"]},"owner_note":{"type":"string","maxLength":2000,"description":"the owner's private note on the enquiry"}},"required":["enquiry_id","status"],"additionalProperties":false}`),
	},
	{
		Name:        "company_order_status",
		Description: "Set an order's status. Needs the owner's 'Change order status' tick for this device; every call is approved by a human first.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string","description":"the order id (UUID) from company_orders"},"status":{"type":"string","enum":["received","accepted","ready","completed","cancelled"]}},"required":["order_id","status"],"additionalProperties":false}`),
	},
}

var (
	companyUUIDRe       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	companyEnquiryState = map[string]bool{"new": true, "contacted": true, "confirmed": true, "closed": true, "declined": true}
	companyOrderState   = map[string]bool{"received": true, "accepted": true, "ready": true, "completed": true, "cancelled": true}
)

const (
	companyOwnerNoteMax     = 2000
	companyGrantWarnBefore  = 3 * 24 * time.Hour
	companyWriteCallTimeout = 30 * time.Second
)

type companyWriteTools struct {
	w         companyWriter
	companyID string
	now       func() time.Time
}

func newCompanyWriteTools(w companyWriter, companyID string) *companyWriteTools {
	return &companyWriteTools{w: w, companyID: companyID, now: time.Now}
}

// missingTick is the refusal text: a sentence for the owner, then one fixed
// clause the desktop app parses. The clause carries a dashboard PATH, never a
// host: the app builds the link from its own server_url (ledger 157).
func (t *companyWriteTools) missingTick(tool string) string {
	a := companyWriteAction[tool]
	return fmt.Sprintf("This device may not %s: the owner turns on '%s' for this device under Connected devices on the company's Overview on rysh.ai. "+
		"missing permission %s (allow it at /ai-native-company/%s/overview#connected-devices)", a.verb, a.label, a.permission, t.companyID)
}

func strictDecode(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (t *companyWriteTools) call(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	action, known := companyWriteAction[name]
	if !known {
		return fmt.Sprintf("unknown tool %q", name), true
	}
	// Validate everything before any request.
	var write func() (json.RawMessage, error)
	switch name {
	case "company_enquiry_update":
		var in struct {
			EnquiryID string  `json:"enquiry_id"`
			Status    string  `json:"status"`
			OwnerNote *string `json:"owner_note"`
		}
		if err := strictDecode(args, &in); err != nil {
			return "invalid arguments: " + err.Error(), true
		}
		switch {
		case !companyUUIDRe.MatchString(in.EnquiryID):
			return "invalid arguments: enquiry_id must be the enquiry's id (a UUID)", true
		case !companyEnquiryState[in.Status]:
			return "invalid arguments: status must be one of new, contacted, confirmed, closed, declined", true
		case in.OwnerNote != nil && len(*in.OwnerNote) > companyOwnerNoteMax:
			return fmt.Sprintf("invalid arguments: owner_note is longer than %d characters", companyOwnerNoteMax), true
		}
		write = func() (json.RawMessage, error) { return t.w.UpdateEnquiry(ctx, in.EnquiryID, in.Status, in.OwnerNote) }
	case "company_order_status":
		var in struct {
			OrderID string `json:"order_id"`
			Status  string `json:"status"`
		}
		if err := strictDecode(args, &in); err != nil {
			return "invalid arguments: " + err.Error(), true
		}
		switch {
		case !companyUUIDRe.MatchString(in.OrderID):
			return "invalid arguments: order_id must be the order's id (a UUID)", true
		case !companyOrderState[in.Status]:
			return "invalid arguments: status must be one of received, accepted, ready, completed, cancelled", true
		}
		write = func() (json.RawMessage, error) { return t.w.SetOrderStatus(ctx, in.OrderID, in.Status) }
	}

	// Ask the server first: the key's own permissions (read-only).
	perms, err := t.w.Permissions(ctx)
	if err != nil {
		return "could not read this device's permissions: " + err.Error(), true
	}
	expires := perms.ExpiresAt(action.permission)
	if !perms.Has(action.permission) || (!expires.IsZero() && !t.now().Before(expires)) {
		return t.missingTick(name), true
	}

	raw, err := write()
	var pe *company.PermissionError
	switch {
	case errors.As(err, &pe):
		return t.missingTick(name), true
	case errors.Is(err, company.ErrReadOnly):
		return "the server does not allow this write for connect keys", true
	case err != nil:
		return "the write failed: " + err.Error(), true
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return "the server answered with invalid JSON", true
	}
	if !expires.IsZero() {
		fmt.Fprintf(&buf, "\npermission %s expires %s", action.permission, expires.UTC().Format("2006-01-02 15:04Z"))
		if expires.Sub(t.now()) < companyGrantWarnBefore {
			buf.WriteString(" (expires soon: the owner renews it under Connected devices)")
		}
	}
	return buf.String(), false
}

func serveCompanyWriteMCP(in io.Reader, out io.Writer, tools *companyWriteTools) error {
	return serveStdioMCP(in, out, stdioMCPServer{
		name:        "rysh-company-write",
		tools:       func() any { return companyWriteToolSpecs },
		call:        tools.call,
		callTimeout: companyWriteCallTimeout,
	})
}

// clientWriter binds the company client's writes to one company.
type clientWriter struct {
	client    *company.Client
	companyID string
}

func (c clientWriter) Permissions(ctx context.Context) (company.Permissions, error) {
	return c.client.Permissions(ctx, c.companyID)
}

func (c clientWriter) UpdateEnquiry(ctx context.Context, enquiryID, status string, ownerNote *string) (json.RawMessage, error) {
	return c.client.UpdateEnquiry(ctx, c.companyID, enquiryID, status, ownerNote)
}

func (c clientWriter) SetOrderStatus(ctx context.Context, orderID, status string) (json.RawMessage, error) {
	return c.client.SetOrderStatus(ctx, c.companyID, orderID, status)
}
