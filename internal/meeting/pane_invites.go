// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"errors"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Inviting people to a rysh room from the session (design 047): a personal
// invite for one person — a rysh account or, without one, a guest — or the
// shared account invite. The server returns the link once; rysh sends no
// email, the owner passes the link on.

// InviteRequest is what `##meeting invite` asks the rysh server for.
type InviteRequest struct {
	Email string        // personal invite labelled with this address
	Guest bool          // personal invite without an address
	Name  string        // the guest's default display name
	TTL   time.Duration // 0 = the server's default (24 h, never past the room)
}

// Personal reports whether the request is for one person (a /join/ link).
func (r InviteRequest) Personal() bool { return r.Email != "" || r.Guest }

// Invite is one invite as the server reports it. JoinURL (with its secret)
// is only set right after creation.
type Invite struct {
	ID            string `json:"id"`
	TokenPrefix   string `json:"token_prefix"`
	Label         string `json:"label"`
	GuestName     string `json:"guest_name"`
	AllowGuest    bool   `json:"allow_guest"`
	MaxUses       int    `json:"max_uses"`
	Uses          int    `json:"uses"`
	ExpiresAt     string `json:"expires_at"`
	RevokedAt     string `json:"revoked_at,omitempty"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
	JoinURL       string `json:"-"`
	GuestIdentity string `json:"-"`
}

var inviteIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidInviteID reports whether id is an invite id (a UUID) as `invites` lists it.
func ValidInviteID(id string) bool { return inviteIDRE.MatchString(id) }

// ValidInviteEmail accepts a bare address (no display name, no brackets).
func ValidInviteEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && len(s) <= 254
}

// CreateInvite asks the room's server for an invite (owner only).
func CreateInvite(ctx context.Context, ref RoomRef, key string, req InviteRequest) (Invite, error) {
	srv, err := paneServer(ref.Server, key)
	if err != nil {
		return Invite{}, err
	}
	if !liveKitRoomName.MatchString(ref.Room) {
		return Invite{}, errors.New("invite: invalid room id")
	}
	body := map[string]any{}
	if req.Email != "" {
		body["email"] = req.Email
	}
	if req.Guest {
		body["guest"] = true
	}
	if req.Name != "" {
		body["name"] = req.Name
	}
	if req.TTL > 0 {
		body["ttl_minutes"] = int(req.TTL / time.Minute)
	}
	var got struct {
		Invite        Invite `json:"invite"`
		JoinURL       string `json:"join_url"`
		GuestIdentity string `json:"guest_identity"`
	}
	if err := srv.call(ctx, "POST", "/api/meeting-rooms/"+ref.Room+"/invites", "invite", body, &got); err != nil {
		return Invite{}, err
	}
	u, err := url.Parse(got.JoinURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || !strings.Contains(u.Fragment, "invite=") || got.Invite.ID == "" {
		return Invite{}, errors.New("invite: unexpected rysh server response")
	}
	if req.Personal() && !strings.HasPrefix(got.GuestIdentity, "guest-") {
		// A server from before design 047 ignores email/guest and returns an
		// account-only invite: say so rather than hand out the wrong link.
		return Invite{}, errors.New("invite: this rysh server does not support guest invites yet; use ##meeting invite <id> link (rysh accounts only)")
	}
	inv := got.Invite
	inv.JoinURL, inv.GuestIdentity = got.JoinURL, got.GuestIdentity
	return inv, nil
}

// ListInvites lists the room's invites, newest first (owner only).
func ListInvites(ctx context.Context, ref RoomRef, key string) ([]Invite, error) {
	srv, err := paneServer(ref.Server, key)
	if err != nil {
		return nil, err
	}
	if !liveKitRoomName.MatchString(ref.Room) {
		return nil, errors.New("invites: invalid room id")
	}
	var got struct {
		Invites []Invite `json:"invites"`
	}
	if err := srv.call(ctx, "GET", "/api/meeting-rooms/"+ref.Room+"/invites", "invites", nil, &got); err != nil {
		return nil, err
	}
	return got.Invites, nil
}

// RevokeInvite revokes one invite; the server also takes its guest out of
// the room.
func RevokeInvite(ctx context.Context, ref RoomRef, key, inviteID string) error {
	srv, err := paneServer(ref.Server, key)
	if err != nil {
		return err
	}
	if !liveKitRoomName.MatchString(ref.Room) || !ValidInviteID(inviteID) {
		return errors.New("uninvite: invalid room or invite id")
	}
	return srv.call(ctx, "DELETE", "/api/meeting-rooms/"+ref.Room+"/invites/"+inviteID, "uninvite", nil, nil)
}
