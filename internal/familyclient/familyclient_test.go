// SPDX-License-Identifier: Apache-2.0

package familyclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/familyclient"
	"github.com/rysh-ai/rysh-cli-code/internal/familyclient/familytest"
)

func newClient(s *familytest.Server) *familyclient.Client {
	return familyclient.New(s.URL, familyclient.NewAuth(s.AccessToken(), "ref-1"))
}

var fastFollow = familyclient.FollowOptions{MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}

func TestFamiliesAndShowFormatting(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	c := newClient(s)
	ctx := context.Background()

	fams, err := c.Families(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ls bytes.Buffer
	familyclient.FormatFamilies(&ls, fams)
	got := ls.String()
	for _, want := range []string{
		"Home  (fam-aaaa, cozy, 2 avatars)",
		"Holiday Crew  (fam-aabb, garden, 1 avatar)",
		"Mia  partner   thinking 40%  looking up concerts",
		"Rex  research  idle",
		"Gigi  gigs  idle",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ls output missing %q:\n%s", want, got)
		}
	}

	fam, err := c.Family(ctx, "fam-aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := c.Tasks(ctx, fam.ID)
	if err != nil {
		t.Fatal(err)
	}
	var show bytes.Buffer
	familyclient.FormatFamily(&show, fam, tasks)
	got = show.String()
	for _, want := range []string{
		"Home  id fam-aaaa-1111  theme cozy",
		"AVATAR  KIND      STATUS    PCT  ID        NOTE",
		"Mia     partner   thinking  40%  av-mia-0  looking up concerts",
		"Rex     research  idle      -    av-rex-0",
		"open tasks: 2",
		"Mia  doing  40%  Find a concert Saturday",
		"Rex  open   -    Compare flights",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Old finished task") {
		t.Errorf("a done task was listed as open:\n%s", got)
	}

	var empty bytes.Buffer
	familyclient.FormatFamilies(&empty, nil)
	if !strings.Contains(empty.String(), "no families yet") {
		t.Errorf("empty list rendered as %q", empty.String())
	}
}

func TestResolveFamilyAndAvatar(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	fams, err := newClient(s).Families(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"fam-aaaa-1111": "Home", // exact id
		"home":          "Home", // case-insensitive name
		"HOLIDAY CREW":  "Holiday Crew",
		"fam-aaaa":      "Home", // unique id prefix
		"fam-aab":       "Holiday Crew",
	} {
		f, err := familyclient.ResolveFamily(fams, q)
		if err != nil || f.Name != want {
			t.Errorf("ResolveFamily(%q) = %q, %v; want %q", q, f.Name, err, want)
		}
	}

	_, err = familyclient.ResolveFamily(fams, "fam-a")
	var amb *familyclient.AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("ambiguous prefix: got %v, want AmbiguousError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `family "fam-a" is ambiguous`) || !strings.Contains(msg, "Home (fam-aaaa-1111)") || !strings.Contains(msg, "Holiday Crew (fam-aabb-2222)") {
		t.Errorf("ambiguous error does not name the candidates: %s", msg)
	}

	_, err = familyclient.ResolveFamily(fams, "nope")
	var nf *familyclient.NotFoundError
	if !errors.As(err, &nf) || !strings.Contains(err.Error(), "known: ") {
		t.Errorf("unknown family: got %v", err)
	}

	home := fams[0]
	for q, want := range map[string]string{"mia": "Mia", "av-rex": "Rex", "av-mia-0001": "Mia"} {
		a, err := familyclient.ResolveAvatar(home, q)
		if err != nil || a.Name != want {
			t.Errorf("ResolveAvatar(%q) = %q, %v; want %q", q, a.Name, err, want)
		}
	}
	if _, err := familyclient.ResolveAvatar(home, "av-"); !errors.As(err, &amb) {
		t.Errorf("ambiguous avatar prefix: got %v", err)
	}

	// Two avatars sharing a name are ambiguous by name, not silently the first.
	dup := familyclient.Family{Avatars: []familyclient.Avatar{{ID: "a1", Name: "Sam"}, {ID: "b2", Name: "sam"}}}
	if _, err := familyclient.ResolveAvatar(dup, "SAM"); !errors.As(err, &amb) {
		t.Errorf("duplicate names: got %v, want ambiguous", err)
	}
}

func TestRefreshOn401ThenRetry(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	auth := familyclient.NewAuth("acc-1", "ref-1")
	var saved []familyclient.Tokens
	auth.OnRefresh = func(tk familyclient.Tokens) error { saved = append(saved, tk); return nil }
	c := familyclient.New(s.URL, auth)

	s.Expire() // the server no longer accepts acc-1
	fams, err := c.Families(context.Background())
	if err != nil {
		t.Fatalf("Families after expiry: %v", err)
	}
	if len(fams) != 2 {
		t.Fatalf("got %d families", len(fams))
	}
	if s.Refreshes() != 1 || auth.Access() != "acc-2" {
		t.Fatalf("refresh calls %d, access %q; want 1, acc-2", s.Refreshes(), auth.Access())
	}
	if len(saved) != 1 || saved[0].AccessToken != "acc-2" || saved[0].RefreshToken != "ref-2" {
		t.Fatalf("OnRefresh got %+v", saved)
	}

	// A token without a refresh token (--token-env) gets the 401 as final.
	s.Expire()
	static := familyclient.New(s.URL, familyclient.NewAuth("acc-2", ""))
	_, err = static.Families(context.Background())
	var ae *familyclient.APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("static token after expiry: got %v, want 401", err)
	}
	if !strings.Contains(err.Error(), "rysh family login") {
		t.Errorf("401 error does not say how to sign in: %v", err)
	}
}

func TestLogin(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	c := familyclient.New(s.URL, nil)
	tk, err := c.Login(context.Background(), "u@example.test", "pw")
	if err != nil || tk.AccessToken != "acc-1" || tk.RefreshToken != "ref-1" {
		t.Fatalf("login: %+v, %v", tk, err)
	}
	if _, err := c.Login(context.Background(), "u@example.test", "wrong"); err == nil || !strings.Contains(err.Error(), "wrong email or password") {
		t.Fatalf("bad password: %v", err)
	}
}

func TestChatStreamsUntilReplyDone(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	s.ReplyNote = "av-rex-0002"
	c := newClient(s)
	fams, _ := c.Families(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := c.OpenChat(ctx, fams[0], fastFollow)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	var out, info bytes.Buffer
	final, err := ch.Ask(ctx, "av-mia-0001", "find a concert", &out, &info)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello there\n" {
		t.Errorf("streamed %q, want %q", out.String(), "Hello there\n")
	}
	if final.Body != "Hello there" || final.Direction != "avatar" {
		t.Errorf("final message %+v", final)
	}
	if !strings.Contains(info.String(), "note     Mia → Rex: can you help?") {
		t.Errorf("note not reported: %q", info.String())
	}
	if p := s.PostedBodies(); len(p) != 1 || p[0] != "find a concert" {
		t.Errorf("posted %q", p)
	}

	// A second turn on the same socket works, and a reply whose stored body
	// extends what streamed prints only the missing tail.
	s.Mu.Lock()
	s.ReplyDeltas = []string{"Sure"}
	s.ReplyFinal = "Sure, Saturday."
	s.Mu.Unlock()
	out.Reset()
	if _, err := ch.Ask(ctx, "av-mia-0001", "again", &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Sure, Saturday.\n" {
		t.Errorf("second turn streamed %q", out.String())
	}
}

func TestChatTimeout(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	s.NoReply = true
	c := newClient(s)
	fams, _ := c.Families(context.Background())
	ch, err := c.OpenChat(context.Background(), fams[0], fastFollow)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = ch.Ask(ctx, "av-mia-0001", "hello?", &bytes.Buffer{}, nil)
	if !errors.Is(err, familyclient.ErrReplyTimeout) {
		t.Fatalf("got %v, want ErrReplyTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestFollowReconnectsAndRefreshesOnHandshake401(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	auth := familyclient.NewAuth("acc-1", "ref-1")
	c := familyclient.New(s.URL, auth)
	s.Expire() // the first handshake is refused with 401

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hellos := make(chan struct{}, 4)
	errc := make(chan error, 1)
	go func() {
		errc <- c.Follow(ctx, "fam-aaaa-1111", fastFollow, func(ev familyclient.Event) {
			if ev.Type == "hello" {
				hellos <- struct{}{}
			}
		})
	}()
	waitHello := func() {
		t.Helper()
		select {
		case <-hellos:
		case err := <-errc:
			t.Fatalf("Follow ended: %v", err)
		case <-ctx.Done():
			t.Fatal("no hello")
		}
	}
	waitHello()
	if s.Refreshes() != 1 || auth.Access() != "acc-2" {
		t.Fatalf("handshake 401 did not refresh: calls %d access %q", s.Refreshes(), auth.Access())
	}
	s.DropSockets()
	waitHello() // reconnected
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Follow returned %v, want context.Canceled", err)
	}
}

func TestFollowGivesUpOnUnknownFamily(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	c := newClient(s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Follow(ctx, "fam-missing", fastFollow, func(familyclient.Event) {})
	if !errors.Is(err, familyclient.ErrSocketGone) {
		t.Fatalf("got %v, want ErrSocketGone", err)
	}
}

func TestEventLines(t *testing.T) {
	pct := 70.0
	from, to := "av-mia-0001", "av-rex-0002"
	fam := familyclient.Family{Name: "Home", Avatars: []familyclient.Avatar{{ID: from, Name: "Mia"}, {ID: to, Name: "Rex"}}}
	f := familyclient.NewEventFormatter(familyclient.Family{})
	f.Clock = func(time.Time) string { return "12:00:00" }
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ev   familyclient.Event
		want string
	}{
		{familyclient.Event{Type: "hello", Family: &fam}, "12:00:00 hello    connected to Home (2 avatars)"},
		{familyclient.Event{Type: "status", AvatarID: from, Status: "working", Pct: &pct, Note: "booking\ntickets"}, "12:00:00 status   Mia → working 70% (booking ↵ tickets)"},
		{familyclient.Event{Type: "note", FromAvatarID: from, ToAvatarID: to, Text: "any gigs Saturday?"}, "12:00:00 note     Mia → Rex: any gigs Saturday?"},
		{familyclient.Event{Type: "task", Task: &familyclient.Task{AvatarID: to, Title: "Compare flights", State: "doing", Pct: &pct}}, "12:00:00 task     Rex [doing 70%] Compare flights"},
		{familyclient.Event{Type: "message", Message: &familyclient.Message{AvatarID: from, Direction: "user", Body: "hi"}}, "12:00:00 message  you → Mia: hi"},
		{familyclient.Event{Type: "message", Message: &familyclient.Message{AvatarID: from, Direction: "avatar", Body: "hello!"}}, "12:00:00 message  Mia: hello!"},
		{familyclient.Event{Type: "message", Message: &familyclient.Message{AvatarID: from, Direction: "avatar_to_avatar", FromAvatarID: &from, ToAvatarID: &to, Body: "help"}}, "12:00:00 message  Mia → Rex: help"},
		{familyclient.Event{Type: "delta", AvatarID: from, Text: "x"}, ""},
		{familyclient.Event{Type: "reply_done", AvatarID: from, Message: &familyclient.Message{Body: "done."}}, "12:00:00 reply    Mia: done."},
		{familyclient.Event{Type: "avatar", Avatar: &familyclient.Avatar{ID: "av-new", Name: "Nan", Kind: "parent", Status: "idle"}}, "12:00:00 avatar   Nan (parent) idle"},
		{familyclient.Event{Type: "status", AvatarID: "av-new", Status: "idle"}, "12:00:00 status   Nan → idle"},
		{familyclient.Event{Type: "avatar_removed", AvatarID: "av-new"}, "12:00:00 avatar   Nan removed"},
		{familyclient.Event{Type: "status", AvatarID: "av-unknown-123", Status: "idle"}, "12:00:00 status   av-unkno → idle"},
		{familyclient.Event{Type: "error", ErrMessage: "pane crashed"}, "12:00:00 error    pane crashed"},
	}
	for i, tc := range cases {
		tc.ev.Received = at
		if got := f.Line(tc.ev); got != tc.want {
			t.Errorf("case %d (%s): got %q, want %q", i, tc.ev.Type, got, tc.want)
		}
	}
}

func TestEventJSONRoundTripsOverloadedMessageKey(t *testing.T) {
	var ev familyclient.Event
	if err := json.Unmarshal([]byte(`{"type":"error","message":"boom"}`), &ev); err != nil || ev.ErrMessage != "boom" || ev.Message != nil {
		t.Fatalf("error frame: %+v %v", ev, err)
	}
	if err := json.Unmarshal([]byte(`{"type":"message","message":{"id":"m1","body":"hi","direction":"user"}}`), &ev); err != nil || ev.Message == nil || ev.Message.Body != "hi" || ev.ErrMessage != "" {
		t.Fatalf("message frame: %+v %v", ev, err)
	}
	b, err := json.Marshal(ev)
	if err != nil || !strings.Contains(string(b), `"message":{"id":"m1"`) {
		t.Fatalf("marshal: %s %v", b, err)
	}
}

func TestTokenStoreIsPrivate(t *testing.T) {
	dir := t.TempDir()
	st := familyclient.DefaultTokenStore(dir)
	if err := st.Put("https://srv.example/", familyclient.StoredSession{Email: "e", AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(st.Path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("token dir mode %v, want 0700", di.Mode().Perm())
	}
	if def, _ := st.DefaultServer(); def != "https://srv.example" {
		t.Errorf("default server %q", def)
	}
	if err := st.UpdateTokens("https://srv.example", familyclient.Tokens{AccessToken: "a2", RefreshToken: "r2"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("https://srv.example")
	if got == nil || got.AccessToken != "a2" || got.RefreshToken != "r2" || got.Email != "e" {
		t.Errorf("after update: %+v", got)
	}
	if had, _ := st.Delete("https://srv.example"); !had {
		t.Error("delete reported nothing to delete")
	}
	if got, _ := st.Get("https://srv.example"); got != nil {
		t.Errorf("session survived delete: %+v", got)
	}
}

// F2 (e20): the Go client authenticates its socket with the Authorization
// header; the token never rides in the URL, where access logs keep it.
func TestFollowSendsTheTokenInAHeaderNotTheURL(t *testing.T) {
	s := familytest.New()
	defer s.Close()
	c := familyclient.New(s.URL, familyclient.NewAuth(s.AccessToken(), "ref-1"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hello := make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- c.Follow(ctx, "fam-aaaa-1111", fastFollow, func(ev familyclient.Event) {
			if ev.Type == "hello" {
				select {
				case hello <- struct{}{}:
				default:
				}
			}
		})
	}()
	select {
	case <-hello:
	case err := <-errc:
		t.Fatalf("Follow ended: %v", err)
	case <-ctx.Done():
		t.Fatal("no hello")
	}
	cancel()
	<-errc
	if n := s.QueryTokenRequests(); n != 0 {
		t.Fatalf("%d request(s) carried the token in the URL, want 0", n)
	}
}

// F1 (e20): plain http/ws only to a loopback host; everything else must be
// https/wss, absolute, and carry no user info.
func TestValidateServer(t *testing.T) {
	for _, ok := range []string{
		"https://dev.rysh.ai", "https://dev.rysh.ai/", "wss://dev.rysh.ai",
		"http://127.0.0.1:8080", "http://localhost:3000", "http://[::1]:9", "ws://127.0.0.1:1",
	} {
		if err := familyclient.ValidateServer(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://dev.rysh.ai", "ws://dev.rysh.ai", "http://localhost.example.test", "http://127.0.0.1.example.test",
		"dev.rysh.ai", "ftp://dev.rysh.ai", "https://user:pw@dev.rysh.ai", "", "https://",
	} {
		if err := familyclient.ValidateServer(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
