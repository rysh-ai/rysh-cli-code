package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Cookie is the subset of CDP's Network.Cookie that we transfer into the
// desktop app's Electron session partition. Field names/tags mirror the CDP
// response so they unmarshal directly from Storage.getCookies; the JSON tags
// are also what we send over the message bus to the app.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // unix seconds; <=0 means a session cookie
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"` // "Strict" | "Lax" | "None" | "" (unspecified)
}

// AllCookies returns every cookie in the browser's default context via
// Storage.getCookies. Unlike document.cookie this includes httpOnly cookies —
// which is exactly the set that matters for a Google login (SID, SAPISID,
// __Secure-1PSID, …). Sent at the browser level (no attached tab needed).
func (b *Browser) AllCookies() ([]Cookie, error) {
	res, err := b.call("", "Storage.getCookies", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("cdp: parse Storage.getCookies: %w", err)
	}
	return out.Cookies, nil
}

// ExtractCookies launches a headless Chromium on an existing profile directory
// and reads its cookie jar. The dir must already hold a logged-in session
// (populated by `##web headless login <profile> <url>` in real Chrome). We only
// read here — no navigation, no sign-in — so automation detection is irrelevant.
//
// Fails clearly if the profile dir is missing (never logged in) or still locked
// by an open login window / live headless browser.
func ExtractCookies(ctx context.Context, userDataDir string) ([]Cookie, error) {
	if fi, err := os.Stat(userDataDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("no logged-in profile at %s — run `##web headless login <profile> <url>` first", userDataDir)
	}
	b, err := Launch(ctx, LaunchOptions{Headless: true, UserDataDir: userDataDir, URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("open profile for cookie read (close any open login window first): %w", err)
	}
	defer b.Close()
	return b.AllCookies()
}

// GoogleIdentityCookies keeps only the cookies that belong to Google's identity
// domains — the ones a third-party "Sign in with Google" flow consults. Dropping
// everything else keeps the transfer tight and avoids importing unrelated jars.
func GoogleIdentityCookies(all []Cookie) []Cookie {
	out := make([]Cookie, 0, len(all))
	for _, c := range all {
		d := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		if d == "google.com" || strings.HasSuffix(d, ".google.com") ||
			d == "youtube.com" || strings.HasSuffix(d, ".youtube.com") ||
			d == "googleusercontent.com" || strings.HasSuffix(d, ".googleusercontent.com") {
			out = append(out, c)
		}
	}
	return out
}
