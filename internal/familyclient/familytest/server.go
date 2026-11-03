// SPDX-License-Identifier: Apache-2.0

// Package familytest is an in-process fake of rysh-server's /api/family
// routes and family socket (rysh-family docs/API.md), for tests only. It is
// imported by the familyclient and cmd/rysh tests and by nothing else.
package familytest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/familyclient"
)

// Server is the fake. Lock Mu before touching its fields from a test while
// requests may be in flight.
type Server struct {
	*httptest.Server

	Mu       sync.Mutex
	Families []familyclient.Family
	Tasks    map[string][]familyclient.Task // by family id

	// Login credentials and the current token pair. A refresh rotates both.
	Email, Password string
	Access, Refresh string
	gen             int

	// Reply drives the scripted reply to a posted message: these deltas,
	// then reply_done with ReplyFinal (default: the deltas joined). NoReply
	// sends nothing (timeout tests). ReplyNote, when set, emits a `note`
	// from the addressed avatar to that avatar id before the reply.
	ReplyDeltas []string
	ReplyFinal  string
	NoReply     bool
	ReplyNote   string

	// Counters for assertions.
	RefreshCalls int
	Unauthorized int
	WSConnects   int
	QueryTokens  int      // requests that carried ?token= (access logs keep URLs)
	Posted       []string // message bodies, in order

	conns    map[*websocket.Conn]string // conn → family id
	connMu   sync.Mutex
	upgrader websocket.Upgrader
	msgSeq   int
}

// New starts a fake with one family "Home" (avatars Mia/partner and
// Rex/research) plus a second family "Holiday Crew", and tokens acc-1/ref-1.
func New() *Server {
	pct := 40.0
	s := &Server{
		Email: "u@example.test", Password: "pw",
		Access: "acc-1", Refresh: "ref-1", gen: 1,
		Families: []familyclient.Family{
			{ID: "fam-aaaa-1111", Name: "Home", Theme: "cozy", Avatars: []familyclient.Avatar{
				{ID: "av-mia-0001", FamilyID: "fam-aaaa-1111", Name: "Mia", Kind: "partner", SeatIndex: 0, Status: "thinking", Pct: &pct, Note: "looking up concerts"},
				{ID: "av-rex-0002", FamilyID: "fam-aaaa-1111", Name: "Rex", Kind: "research", SeatIndex: 1, Status: "idle"},
			}},
			{ID: "fam-aabb-2222", Name: "Holiday Crew", Theme: "garden", Avatars: []familyclient.Avatar{
				{ID: "av-gig-0003", FamilyID: "fam-aabb-2222", Name: "Gigi", Kind: "gigs", Status: "idle"},
			}},
		},
		Tasks: map[string][]familyclient.Task{
			"fam-aaaa-1111": {
				{ID: "t1", FamilyID: "fam-aaaa-1111", AvatarID: "av-mia-0001", Title: "Find a concert Saturday", State: "doing", Pct: &pct},
				{ID: "t2", FamilyID: "fam-aaaa-1111", AvatarID: "av-rex-0002", Title: "Old finished task", State: "done"},
				{ID: "t3", FamilyID: "fam-aaaa-1111", AvatarID: "av-rex-0002", Title: "Compare flights", State: "open"},
			},
		},
		ReplyDeltas: []string{"Hel", "lo ", "there"},
		conns:       map[*websocket.Conn]string{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Expire invalidates the current access token (as if it timed out), so the
// next request gets 401 until the client refreshes.
func (s *Server) Expire() {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Access = "expired-" + s.Access
}

// AccessToken is the currently valid access token.
func (s *Server) AccessToken() string {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.Access
}

// Broadcast sends ev to every socket on family fid.
func (s *Server) Broadcast(fid string, ev familyclient.Event) {
	b, _ := json.Marshal(ev)
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c, f := range s.conns {
		if f == fid {
			_ = c.WriteMessage(websocket.TextMessage, b)
		}
	}
}

// DropSockets closes every open socket (reconnect tests).
func (s *Server) DropSockets() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c := range s.conns {
		_ = c.Close()
		delete(s.conns, c)
	}
}

// Refreshes is the number of /api/auth/refresh calls so far.
func (s *Server) Refreshes() int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.RefreshCalls
}

// PostedBodies is a copy of the message bodies posted so far.
func (s *Server) PostedBodies() []string {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return append([]string(nil), s.Posted...)
}

// SocketCount is the number of open sockets.
func (s *Server) SocketCount() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func (s *Server) authorized(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if q := r.URL.Query().Get("token"); q != "" {
		s.QueryTokens++
		if tok == "" {
			tok = q
		}
	}
	ok := tok != "" && tok == s.Access
	if !ok {
		s.Unauthorized++
	}
	return ok
}

func (s *Server) family(fid string) (familyclient.Family, bool) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	for _, f := range s.Families {
		if f.ID == fid {
			return f, true
		}
	}
	return familyclient.Family{}, false
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/auth/login" && r.Method == http.MethodPost:
		var in struct{ Email, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.Mu.Lock()
		ok := in.Email == s.Email && in.Password == s.Password
		t := familyclient.Tokens{AccessToken: s.Access, RefreshToken: s.Refresh, ExpiresIn: 900}
		s.Mu.Unlock()
		if !ok {
			apiErr(w, 401, "invalid credentials")
			return
		}
		writeJSON(w, 200, t)
		return
	case p == "/api/auth/refresh" && r.Method == http.MethodPost:
		var in struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.Mu.Lock()
		s.RefreshCalls++
		if in.RefreshToken != s.Refresh {
			s.Mu.Unlock()
			apiErr(w, 401, "invalid refresh token")
			return
		}
		s.gen++
		s.Access = "acc-" + itoa(s.gen)
		s.Refresh = "ref-" + itoa(s.gen)
		t := familyclient.Tokens{AccessToken: s.Access, RefreshToken: s.Refresh, ExpiresIn: 900}
		s.Mu.Unlock()
		writeJSON(w, 200, t)
		return
	}

	if !strings.HasPrefix(p, "/api/family/") {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		apiErr(w, 401, "unauthorized")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(p, "/api/family/"), "/"), "/")
	// families | families/:fid | families/:fid/tasks | families/:fid/ws |
	// families/:fid/avatars/:aid/messages
	if parts[0] != "families" {
		apiErr(w, 404, "not_found")
		return
	}
	if len(parts) == 1 {
		s.Mu.Lock()
		fams := append([]familyclient.Family{}, s.Families...)
		s.Mu.Unlock()
		writeJSON(w, 200, fams)
		return
	}
	fam, ok := s.family(parts[1])
	if !ok {
		apiErr(w, 404, "not_found")
		return
	}
	switch {
	case len(parts) == 2:
		writeJSON(w, 200, fam)
	case len(parts) == 3 && parts[2] == "tasks":
		s.Mu.Lock()
		t := append([]familyclient.Task{}, s.Tasks[fam.ID]...)
		s.Mu.Unlock()
		writeJSON(w, 200, t)
	case len(parts) == 3 && parts[2] == "ws":
		s.serveWS(w, r, fam)
	case len(parts) == 5 && parts[2] == "avatars" && parts[4] == "messages":
		aid := parts[3]
		if r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{"messages": []familyclient.Message{}})
			return
		}
		var in struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.Mu.Lock()
		s.msgSeq++
		m := familyclient.Message{ID: "m" + itoa(s.msgSeq), FamilyID: fam.ID, AvatarID: aid, Direction: "user", Body: in.Body}
		s.Posted = append(s.Posted, in.Body)
		s.Mu.Unlock()
		writeJSON(w, 200, map[string]any{"message": m})
		go s.reply(fam.ID, aid, m)
	default:
		apiErr(w, 404, "not_found")
	}
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request, fam familyclient.Family) {
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.Mu.Lock()
	s.WSConnects++
	s.Mu.Unlock()
	hello, _ := json.Marshal(familyclient.Event{Type: "hello", Family: &fam})
	s.connMu.Lock()
	_ = c.WriteMessage(websocket.TextMessage, hello)
	s.conns[c] = fam.ID
	s.connMu.Unlock()
	go func() { // read pings until the client leaves
		defer func() {
			s.connMu.Lock()
			delete(s.conns, c)
			s.connMu.Unlock()
			c.Close()
		}()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (s *Server) reply(fid, aid string, user familyclient.Message) {
	time.Sleep(10 * time.Millisecond)
	s.Mu.Lock()
	deltas := append([]string{}, s.ReplyDeltas...)
	final, none, note := s.ReplyFinal, s.NoReply, s.ReplyNote
	s.msgSeq++
	rid := "m" + itoa(s.msgSeq)
	s.Mu.Unlock()
	s.Broadcast(fid, familyclient.Event{Type: "message", Message: &user})
	if none {
		return
	}
	s.Broadcast(fid, familyclient.Event{Type: "status", AvatarID: aid, Status: "thinking"})
	if note != "" {
		s.Broadcast(fid, familyclient.Event{Type: "note", FromAvatarID: aid, ToAvatarID: note, Text: "can you help?"})
	}
	// Another avatar's chunk must never leak into this reply.
	s.Broadcast(fid, familyclient.Event{Type: "delta", AvatarID: "someone-else", ReplyID: "x", Text: "NOISE"})
	for _, d := range deltas {
		s.Broadcast(fid, familyclient.Event{Type: "delta", AvatarID: aid, ReplyID: rid, Text: d})
	}
	if final == "" {
		final = strings.Join(deltas, "")
	}
	msg := familyclient.Message{ID: rid, FamilyID: fid, AvatarID: aid, Direction: "avatar", Body: final}
	s.Broadcast(fid, familyclient.Event{Type: "reply_done", AvatarID: aid, ReplyID: rid, Message: &msg})
	s.Broadcast(fid, familyclient.Event{Type: "status", AvatarID: aid, Status: "idle"})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// QueryTokenRequests is how many requests carried the token in the URL.
func (s *Server) QueryTokenRequests() int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.QueryTokens
}
