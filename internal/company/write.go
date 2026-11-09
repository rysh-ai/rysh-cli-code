// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// CD5 (sprint 0.2.16): named write actions a connect key may hold once the
// owner ticks them for the device under "Connected devices". Default none.

// ErrReadOnly is a write the connect key can never make (403 code read_only).
var ErrReadOnly = errors.New("connect keys are read-only for this action")

// PermissionError is a write refused because the owner has not granted (or
// no longer grants) this device the named permission (403 code
// permission_required).
type PermissionError struct{ Permission string }

func (e *PermissionError) Error() string { return "missing permission " + e.Permission }

// Permissions is the connect key's own grant list (GET .../connect/permissions).
type Permissions struct {
	List     []string             `json:"permissions"`
	ExpireAt map[string]time.Time `json:"permissions_expire_at"`
}

// Has reports whether name is granted (expiry is checked by the caller).
func (p Permissions) Has(name string) bool {
	for _, n := range p.List {
		if n == name {
			return true
		}
	}
	return false
}

// ExpiresAt is when the grant name lapses (zero when unknown or never).
func (p Permissions) ExpiresAt(name string) time.Time { return p.ExpireAt[name] }

// Permissions reads the key's own permissions. Read-only.
func (c *Client) Permissions(ctx context.Context, companyID string) (Permissions, error) {
	var p Permissions
	err := c.get(ctx, "/api/ai-native-companies/"+url.PathEscape(companyID)+"/connect/permissions", &p)
	return p, err
}

// UpdateEnquiry sets an enquiry's status and, when ownerNote is non-nil, its
// owner note (permission enquiry.update; nothing leaves rysh).
func (c *Client) UpdateEnquiry(ctx context.Context, companyID, enquiryID, status string, ownerNote *string) (json.RawMessage, error) {
	body := map[string]any{"status": status}
	if ownerNote != nil {
		body["owner_note"] = *ownerNote
	}
	var raw json.RawMessage
	err := c.do(ctx, "PATCH", "/api/ai-native-companies/"+url.PathEscape(companyID)+"/enquiries/"+url.PathEscape(enquiryID), body, &raw)
	return raw, err
}

// SetOrderStatus sets an order's status (permission order.status).
func (c *Client) SetOrderStatus(ctx context.Context, companyID, orderID, status string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.do(ctx, "PATCH", "/api/ai-native-companies/"+url.PathEscape(companyID)+"/orders/"+url.PathEscape(orderID), map[string]any{"status": status}, &raw)
	return raw, err
}
