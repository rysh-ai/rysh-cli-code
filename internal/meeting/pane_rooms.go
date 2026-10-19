// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// rysh rooms opened in a meeting pane (design 043 §Command grammar, §Pane
// contract). The daemon talks to its own configured rysh server only: the
// session's rysh key is never sent to a host taken from a pasted link.

// RoomRef is a rysh room a meeting pane shows.
type RoomRef struct {
	Server string // rysh-server base, e.g. https://rysh.ai
	Room   string // room id
	// ExpiresAt is when the room ends (RFC 3339), set by CreateRoom only.
	ExpiresAt string
}

// MeetLink is the /meet/<room> page people open to join the room.
func (r RoomRef) MeetLink() string { return r.Server + "/meet/" + r.Room }

// ParseRoomRef reads `rysh:<room-id>` or `https://<server>/meet/<room-id>`
// against the session's rysh server. ok is false when arg is neither form
// (it may be a Meet/Zoom/Teams link); err is set when it is one of them but
// unusable — a /meet/ link on another host is refused rather than followed,
// because following it would send the session's key there.
func ParseRoomRef(arg, server string) (ref RoomRef, ok bool, err error) {
	if id, found := strings.CutPrefix(arg, "rysh:"); found {
		if !liveKitRoomName.MatchString(id) {
			return RoomRef{}, true, errors.New("rysh:<room-id>: invalid room id")
		}
		base, err := sessionServer(server)
		return RoomRef{Server: base, Room: id}, true, err
	}
	if _, err := MeetingPlatform(arg); err == nil {
		// Teams short links are /meet/<id> too (teams.microsoft.com,
		// teams.live.com): a platform link is never a rysh room.
		return RoomRef{}, false, nil
	}
	u, perr := url.Parse(arg)
	if perr != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.HasPrefix(u.Path, "/meet/") {
		return RoomRef{}, false, nil
	}
	id := strings.TrimPrefix(u.Path, "/meet/")
	if !liveKitRoomName.MatchString(id) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return RoomRef{}, true, errors.New("use a rysh room link: https://<rysh server>/meet/<room-id>")
	}
	if strings.TrimSpace(server) == "" {
		return RoomRef{}, true, errors.New("that room is on " + u.Host + ", which is not this session's rysh server (this session has none; connect it upstream or set the session var RYSH_SERVER_URL)")
	}
	base, err := sessionServer(server)
	if err != nil {
		return RoomRef{}, true, err
	}
	if b, _ := url.Parse(base); b == nil || !strings.EqualFold(b.Host, u.Host) || b.Scheme != u.Scheme {
		return RoomRef{}, true, errors.New("that room is on " + u.Host + ", not on this session's rysh server " + base + "; rysh opens rooms on its own server only")
	}
	return RoomRef{Server: base, Room: id}, true, nil
}

func sessionServer(server string) (string, error) {
	if strings.TrimSpace(server) == "" {
		return "", errors.New("this session has no rysh server; connect it (upstream) or set the session var RYSH_SERVER_URL")
	}
	base, err := validServerURL(server)
	if err != nil {
		return "", errors.New("rysh server " + server + ": " + err.Error())
	}
	return base, nil
}

func paneServer(server, key string) (ryshServer, error) {
	base, err := sessionServer(server)
	if err != nil {
		return ryshServer{}, err
	}
	if key == "" {
		return ryshServer{}, errors.New("set " + RyshAPIKeySecret + " (a rysh API key with write permission) in this session's secrets, or connect the session to the server")
	}
	return ryshServer{base: base, key: key, http: newHTTPClient()}, nil
}

// CheckRoom confirms the room exists and is readable with the session key.
func CheckRoom(ctx context.Context, ref RoomRef, key string) error {
	srv, err := paneServer(ref.Server, key)
	if err != nil {
		return err
	}
	_, err = srv.getServerRoom(ctx, ref.Room)
	return err
}

// CreateRoom opens a new room on the session's rysh server for a meeting
// pane (`##meeting join new`). The room lives for ttl (rounded up to hours).
func CreateRoom(ctx context.Context, server, key, title string, ttl time.Duration) (RoomRef, error) {
	srv, err := paneServer(server, key)
	if err != nil {
		return RoomRef{}, err
	}
	if title == "" {
		title = "Rysh meeting"
	}
	hours := max(1, int((ttl+time.Hour-1)/time.Hour))
	room, err := srv.createServerRoom(ctx, title, hours)
	if err != nil {
		return RoomRef{}, err
	}
	return RoomRef{Server: srv.base, Room: room.ID, ExpiresAt: room.ExpiresAt}, nil
}

// ParticipantGrant is the human participant's LiveKit access for a meeting
// pane. It is handed to the desktop app and never stored or logged.
type ParticipantGrant struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	Identity  string `json:"identity"`
	ExpiresAt string `json:"expires_at"`
}

// ParticipantToken asks the rysh server for a participant token for the key's
// account in ref's room (POST /api/meeting-rooms/<room>/token).
func ParticipantToken(ctx context.Context, ref RoomRef, key string) (ParticipantGrant, error) {
	srv, err := paneServer(ref.Server, key)
	if err != nil {
		return ParticipantGrant{}, err
	}
	if !liveKitRoomName.MatchString(ref.Room) {
		return ParticipantGrant{}, errors.New("token: invalid room id")
	}
	var got struct {
		Token     string `json:"token"`
		URL       string `json:"url"`
		Identity  string `json:"identity"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := srv.call(ctx, "POST", "/api/meeting-rooms/"+ref.Room+"/token", "token", nil, &got); err != nil {
		return ParticipantGrant{}, err
	}
	if got.Token == "" || got.Identity == "" {
		return ParticipantGrant{}, errors.New("token: unexpected rysh server response")
	}
	if _, _, err := liveKitEndpoints(got.URL); err != nil {
		return ParticipantGrant{}, errors.New("token: rysh server returned an unusable LiveKit URL")
	}
	return ParticipantGrant{URL: got.URL, Token: got.Token, Identity: got.Identity, ExpiresAt: got.ExpiresAt}, nil
}
