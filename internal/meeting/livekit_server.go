// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Server mode of `rysh meeting host --server <rysh-server>` (design 042
// §LK-2/§LK-3). rysh-server owns the room: it creates it for the account
// behind the daemon's API key, mints the bot's agent token and the invite
// link, and decides who may join (accounts only). The daemon holds no
// LiveKit API secret in this mode; the LiveKit URL comes from the server.

// RyshAPIKeySecret is the session secret holding a rysh workspace API key
// (`rysh_…`) with the write permission; without it the daemon's upstream
// key (upstream.api_key) is used.
const RyshAPIKeySecret = "RYSH_API_KEY"

// serverRoom is rysh-server's room object (fields the daemon uses).
type serverRoom struct {
	ID         string `json:"id"`
	OwnerID    string `json:"owner_id"`
	RoomName   string `json:"room_name"`
	LiveKitURL string `json:"livekit_url"`
	ExpiresAt  string `json:"expires_at"`
}

// serverToken is rysh-server's token response.
type serverToken struct {
	Token    string `json:"token"`
	URL      string `json:"url"`
	RoomName string `json:"room_name"`
	Identity string `json:"identity"`
}

// validServerURL accepts an https rysh-server base; plain http only for a
// loopback server (local development).
func validServerURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("--server must be the https URL of a rysh server, e.g. https://rysh.ai")
	}
	switch u.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("--server must use https (plain http only for a local server)")
		}
	default:
		return "", errors.New("--server must be the https URL of a rysh server, e.g. https://rysh.ai")
	}
	return u.String(), nil
}

type ryshServer struct {
	base, key string
	http      *http.Client
}

// serverError maps rysh-server's documented statuses to operator text. The
// body is read only for 402's upgrade_url; nothing else from it is echoed.
func serverError(op string, res *http.Response) error {
	switch res.StatusCode {
	case 401:
		return fmt.Errorf("%s: rysh server rejected the API key (%s)", op, RyshAPIKeySecret)
	case 402:
		var b struct {
			UpgradeURL string `json:"upgrade_url"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&b)
		msg := op + ": this rysh account's plan does not include meetings"
		if u, err := url.Parse(b.UpgradeURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && !strings.ContainsAny(b.UpgradeURL, " \r\n") {
			msg += "; upgrade: " + b.UpgradeURL
		}
		return errors.New(msg)
	case 403:
		// One 403 is not about the key (design 047): a free account hosts
		// once its email is verified. Matched, never echoed.
		var b struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&b)
		if strings.Contains(b.Error, "verify your email") {
			return fmt.Errorf("%s: verify this rysh account's email address to host meetings (free accounts can host once verified)", op)
		}
		return fmt.Errorf("%s: not allowed — the API key needs the write permission and must belong to the room owner", op)
	case 404:
		return fmt.Errorf("%s: room not found", op)
	case 409:
		return fmt.Errorf("%s: this account already has the maximum number of open rooms; end one first", op)
	case 410:
		return fmt.Errorf("%s: the room has ended or expired", op)
	case 503:
		return fmt.Errorf("%s: meetings are not available on this rysh server yet", op)
	}
	return fmt.Errorf("%s: rysh server returned HTTP %d", op, res.StatusCode)
}

func (r ryshServer) call(ctx context.Context, method, path, op string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return errors.New(op + ": invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: rysh server unreachable", op)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return serverError(op, res)
	}
	if out == nil {
		return nil
	}
	if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(out) != nil {
		return fmt.Errorf("%s: unreadable rysh server response", op)
	}
	return nil
}

// serverPreflight checks what every server-mode bot start needs and returns
// the rysh-server client and the bound agents.
func serverPreflight(c Command, keys Credentials, execute Execute, agents []Agent) (ryshServer, []meetingAgent, error) {
	base, err := validServerURL(c.Server)
	if err != nil {
		return ryshServer{}, nil, err
	}
	if keys.RyshAPIKey == "" {
		return ryshServer{}, nil, errors.New("set " + RyshAPIKeySecret + " (a rysh API key with write permission) in this session's secrets, or connect the session to the server")
	}
	if err := roomSpeechKeysError(c, keys); err != nil {
		return ryshServer{}, nil, err
	}
	if execute == nil {
		return ryshServer{}, nil, errors.New("session executor unavailable")
	}
	bound, err := bindAgents(agents, c.TTS)
	if err != nil {
		return ryshServer{}, nil, err
	}
	return ryshServer{base: base, key: keys.RyshAPIKey, http: newHTTPClient()}, bound, nil
}

// createServerRoom is the create-room step: POST /api/meeting-rooms for the
// account behind the key.
func (r ryshServer) createServerRoom(ctx context.Context, title string, hours int) (serverRoom, error) {
	var created struct {
		Room serverRoom `json:"room"`
	}
	if err := r.call(ctx, "POST", "/api/meeting-rooms", "create room", map[string]any{"title": title, "ttl_hours": hours}, &created); err != nil {
		return serverRoom{}, err
	}
	if !liveKitRoomName.MatchString(created.Room.ID) || created.Room.OwnerID == "" {
		return serverRoom{}, errors.New("create room: unexpected rysh server response")
	}
	return created.Room, nil
}

// getServerRoom reads an existing room (GET /api/meeting-rooms/<id>); the
// owner id is the "host" allowlist identity.
func (r ryshServer) getServerRoom(ctx context.Context, roomID string) (serverRoom, error) {
	if !liveKitRoomName.MatchString(roomID) {
		return serverRoom{}, errors.New("room: invalid rysh room id")
	}
	var got struct {
		Room serverRoom `json:"room"`
	}
	if err := r.call(ctx, "GET", "/api/meeting-rooms/"+roomID, "room", nil, &got); err != nil {
		return serverRoom{}, err
	}
	if got.Room.ID != roomID || got.Room.OwnerID == "" {
		return serverRoom{}, errors.New("room: unexpected rysh server response")
	}
	return got.Room, nil
}

// joinServerRoomTransport is the join-room step: POST
// /api/meeting-rooms/<id>/agent-token {agent} and a transport for the bot.
// rysh-server derives the bot's identity ("agent-<agent>") and display name
// (<agent>) from the agent name alone.
func (r ryshServer) joinServerRoomTransport(ctx context.Context, room serverRoom, agent, sttKey string) (*livekitTransport, error) {
	var bot serverToken
	if err := r.call(ctx, "POST", "/api/meeting-rooms/"+room.ID+"/agent-token", "bot token", map[string]string{"agent": agent}, &bot); err != nil {
		return nil, err
	}
	if bot.Token == "" || bot.Identity == "" {
		return nil, errors.New("bot token: unexpected rysh server response")
	}
	if _, _, err := liveKitEndpoints(bot.URL); err != nil {
		return nil, errors.New("bot token: rysh server returned an unusable LiveKit URL")
	}
	name := room.RoomName
	if name == "" {
		name = bot.RoomName
	}
	return &livekitTransport{server: bot.URL, token: bot.Token, room: name, botIdentity: bot.Identity, hostIdentity: room.OwnerID, sttKey: sttKey}, nil
}

// serverAgentName is the rysh-server agent name for a bot carrying these
// agents (design 043 Decision 3): the agent's name when there is one,
// otherwise "rysh". rysh-server accepts only [a-z0-9-]{1,32} starting with
// a letter or digit, so a name is lowercased and other characters become
// dashes; a name that still does not fit falls back to "rysh". The
// "rysh (a, b)" form of Decision 3 cannot be expressed through the
// agent-token endpoint.
func serverAgentName(agents []string) string {
	if len(agents) != 1 {
		return "rysh"
	}
	b := []byte(strings.ToLower(strings.TrimSpace(agents[0])))
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			b[i] = '-'
		}
	}
	name := strings.Trim(string(b), "-")
	if !serverAgentNameRE.MatchString(name) {
		return "rysh"
	}
	return name
}

var serverAgentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// joinServerRoom starts the bot in an EXISTING rysh-server room (the one a
// human opened in a meeting pane, design 043 §Agent-add flows): no room is
// created and no invite is issued, and a failure never ends the room — it
// is not the bot's. c.Server is the rysh-server base; c.Agents names the
// agents the bot carries (agents holds their resolved definitions). Called
// without m.mu (provider I/O); the bot goes into slot.
func (m *Manager) joinServerRoom(slot *botSlot, c Command, keys Credentials, execute Execute, agents []Agent, roomID string) Reply {
	if err := c.ValidateHost(); err != nil {
		return Reply{Error: err.Error()}
	}
	srv, bound, err := serverPreflight(c, keys, execute, agents)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	names := c.Agents
	if len(names) == 0 {
		for _, a := range agents {
			names = append(names, a.Name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	room, err := srv.getServerRoom(ctx, roomID)
	if err != nil {
		return Reply{State: "create_failed", Error: err.Error()}
	}
	t, err := srv.joinServerRoomTransport(ctx, room, serverAgentName(names), keys.LiveKit.STTKey)
	if err != nil {
		return Reply{State: "create_failed", Error: err.Error()}
	}
	c.Room = t.room
	return m.startLiveKit(slot, c, keys, execute, bound, t, srv.base+"/meet/"+room.ID, "The room is unchanged; people in it can keep meeting.")
}

// hostOnServer creates the room on rysh-server, fetches the bot's agent
// token and an invite link, and connects the bot. Called without m.mu.
func (m *Manager) hostOnServer(slot *botSlot, c Command, keys Credentials, execute Execute, agents []Agent) Reply {
	srv, bound, err := serverPreflight(c, keys, execute, agents)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	title := c.Room
	if title == "" {
		title = "Rysh meeting"
	}
	lifetime := time.Duration(c.Minutes)*time.Minute + 10*time.Minute
	hours := int((lifetime + time.Hour - 1) / time.Hour)
	room, err := srv.createServerRoom(ctx, title, hours)
	if err != nil {
		return Reply{State: "create_failed", Error: err.Error()}
	}
	// A room the bot cannot use is ended again, so it does not count
	// against the owner's open-room cap.
	abandon := func(r Reply) Reply {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.call(ctx, "DELETE", "/api/meeting-rooms/"+room.ID, "end room", nil, nil) != nil {
			r.Notice = strings.TrimSpace(r.Notice + " The room could not be ended; it expires on its own.")
		}
		return r
	}
	t, err := srv.joinServerRoomTransport(ctx, room, "rysh", keys.LiveKit.STTKey)
	if err != nil {
		return abandon(Reply{State: "create_failed", Error: err.Error()})
	}
	var invite struct {
		JoinURL string `json:"join_url"`
	}
	minutes := int(lifetime / time.Minute)
	if err := srv.call(ctx, "POST", "/api/meeting-rooms/"+room.ID+"/invites", "invite", map[string]any{"ttl_minutes": minutes, "max_uses": 20}, &invite); err != nil {
		return abandon(Reply{State: "create_failed", Error: err.Error()})
	}
	if u, err := url.Parse(invite.JoinURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return abandon(Reply{State: "create_failed", Error: "invite: unexpected rysh server response"})
	}
	c.Room = t.room
	return m.startLiveKit(slot, c, keys, execute, bound, t, invite.JoinURL, "The room exists on the rysh server and the invite works for people; it ends when it expires or its owner ends it.")
}
