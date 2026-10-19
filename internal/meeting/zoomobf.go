// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Zoom OBF contingency (design 040 §MTG-10). Since 2026 Zoom requires Meeting
// SDK bots joining meetings hosted by another account to present an "on
// behalf of" token minted for a Zoom user who authorised the app and is
// already in the meeting. Recall fetches that token from a URL we give it at
// create (zoom.obf_token_url). Off unless `join --zoom-obf`.

// Secret names; values live only in the session secret store.
const (
	ZoomClientIDSecret     = "ZOOM_OAUTH_CLIENT_ID"
	ZoomClientSecretSecret = "ZOOM_OAUTH_CLIENT_SECRET"
	ZoomRefreshTokenSecret = "ZOOM_OAUTH_REFRESH_TOKEN"
)

// zoomOBFMaxMints bounds how often one meeting may ask for a token: Recall
// asks once per join attempt, so a handful covers retries, not a loop.
const zoomOBFMaxMints = 5

var zoomMeetingID = regexp.MustCompile(`^/(?:j/|wc/join/|wc/)([0-9]{9,12})(?:/join)?$`)

// ZoomCredentials is the Zoom OAuth app (General app with Meeting SDK) and the
// authorising user's refresh token. SaveRefresh persists a rotated refresh
// token: Zoom invalidates the old one on each refresh.
type ZoomCredentials struct {
	ClientID, ClientSecret, RefreshToken string
	SaveRefresh                          func(string) error
}

func (z ZoomCredentials) complete() bool {
	return z.ClientID != "" && z.ClientSecret != "" && z.RefreshToken != ""
}

// zoomMeetingNumber extracts the numeric meeting id OBF tokens are bound to.
// Personal /my/ links carry no id, so they cannot use OBF.
func zoomMeetingNumber(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	m := zoomMeetingID.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

type zoomOBF struct {
	mu         sync.Mutex
	capability string // auth_token Recall echoes back; never the Zoom secret
	meetingID  string
	creds      ZoomCredentials
	oauthURL   string // https://zoom.us/oauth/token
	apiURL     string // https://api.zoom.us/v2
	http       *http.Client
	access     string
	accessExp  time.Time
	mints      int
	saveErr    string // sticky: a rotated refresh token was not persisted
}

func newZoomOBF(meetingURL string, creds ZoomCredentials) (*zoomOBF, error) {
	id, ok := zoomMeetingNumber(meetingURL)
	if !ok {
		return nil, errors.New("--zoom-obf needs a Zoom /j/<id> or /wc/join/<id> link (personal /my/ links carry no meeting id)")
	}
	if !creds.complete() {
		return nil, fmt.Errorf("--zoom-obf needs session secrets %s, %s and %s", ZoomClientIDSecret, ZoomClientSecretSecret, ZoomRefreshTokenSecret)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.New("cannot generate OBF callback capability")
	}
	return &zoomOBF{capability: hex.EncodeToString(b), meetingID: id, creds: creds, oauthURL: "https://zoom.us/oauth/token", apiURL: "https://api.zoom.us/v2", http: newHTTPClient()}, nil
}

// callbackURL is what Recall GETs for a token. The capability rides in the
// query because Recall calls the URL verbatim; it is per-meeting and dies
// with the local bridge.
func (z *zoomOBF) callbackURL(publicURL string) string {
	return publicURL + "/zoom/obf?auth_token=" + z.capability
}

func (z *zoomOBF) authorized(r *http.Request) bool {
	got := r.URL.Query().Get("auth_token")
	return len(got) == len(z.capability) && subtle.ConstantTimeCompare([]byte(got), []byte(z.capability)) == 1
}

// mint returns a fresh OBF token. Serialised: a refresh rotates the refresh
// token, so two concurrent refreshes would invalidate each other.
func (z *zoomOBF) mint(ctx context.Context) (string, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.mints >= zoomOBFMaxMints {
		return "", errors.New("OBF mint limit reached for this meeting")
	}
	z.mints++
	if z.access == "" || time.Now().After(z.accessExp) {
		if err := z.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	q := url.Values{"type": {"onbehalf"}, "meeting_id": {z.meetingID}}
	req, _ := http.NewRequestWithContext(ctx, "GET", z.apiURL+"/users/me/token?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+z.access)
	var out struct {
		Token string `json:"token"`
	}
	if err := z.doJSON(req, &out); err != nil {
		return "", errors.New("Zoom OBF token request " + err.Error())
	}
	if out.Token == "" || len(out.Token) > 8<<10 || strings.ContainsAny(out.Token, "\r\n") {
		return "", errors.New("no usable OBF token from Zoom")
	}
	return out.Token, nil
}

func (z *zoomOBF) refreshLocked(ctx context.Context) error {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {z.creds.RefreshToken}}
	req, _ := http.NewRequestWithContext(ctx, "POST", z.oauthURL, strings.NewReader(form.Encode()))
	req.SetBasicAuth(z.creds.ClientID, z.creds.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := z.doJSON(req, &out); err != nil {
		return errors.New("Zoom OAuth refresh " + err.Error() + "; re-authorise the Zoom user")
	}
	if out.Access == "" {
		return errors.New("no access token in the Zoom OAuth refresh")
	}
	z.access = out.Access
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 || life > time.Hour {
		life = time.Hour
	}
	z.accessExp = time.Now().Add(life - time.Minute)
	if out.Refresh != "" && out.Refresh != z.creds.RefreshToken {
		z.creds.RefreshToken = out.Refresh // in memory first: the old one is already dead
		if z.creds.SaveRefresh == nil || z.creds.SaveRefresh(out.Refresh) != nil {
			z.saveErr = "rotated Zoom refresh token could not be saved; re-authorise the Zoom user after this meeting"
		}
	}
	return nil
}

// doJSON never surfaces a provider body: it may echo tokens.
func (z *zoomOBF) doJSON(req *http.Request, out any) error {
	res, err := z.http.Do(req)
	if err != nil {
		return errors.New("failed (network)")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("returned HTTP %d", res.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(out) != nil {
		return errors.New("returned an unreadable body")
	}
	return nil
}

// serveZoomOBF answers Recall's GET with the token as text/plain.
func (s *Service) serveZoomOBF(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	z, done := s.obf, s.terminal
	s.mu.Unlock()
	if z == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	if !z.authorized(r) {
		w.WriteHeader(401)
		return
	}
	if done {
		w.WriteHeader(410)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	token, err := z.mint(ctx)
	z.mu.Lock()
	saveErr := z.saveErr
	z.mu.Unlock()
	s.mu.Lock()
	if err != nil {
		s.notice = "Zoom OBF: " + err.Error()
	} else if saveErr != "" {
		s.notice = "Zoom OBF: " + saveErr
	} else if strings.HasPrefix(s.notice, "Zoom OBF: ") {
		s.notice = ""
	}
	s.mu.Unlock()
	if err != nil {
		w.WriteHeader(502)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, token)
}
