// SPDX-License-Identifier: Apache-2.0

package main

// rysh connect --file — the connect file half of Company Connect (plan 0.2.16
// S2.5). The company page offers rysh-connect-<slug>.ryshconnect next to the
// rysh:// link (founder-tools' S2.2 endpoint); the desktop app picks it up on
// first launch and a terminal user passes it here. One format, two readers
// (agreed with desktop's app reader, 2026-11-08):
//
//	{"rysh_connect":1,"server":"https://<host>","code":"rysh_ct_<48 hex>",
//	 "company":{"name":"…","slug":"…"},"expires_at":"<RFC3339 UTC>"}
//
// The file carries a server and a one-time code, never a key. Anything else in
// it — an unknown field, a field named like a credential, any rysh_ value but
// the code — refuses the whole file before anything is sent, and no value from
// the file is ever echoed in an error. The file is removed only after a good
// claim: a refused claim keeps it, so the user still holds what they were given.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	connectFileVersion = 1
	connectFileMaxSize = 4 << 10
	connectFileMaxAge  = 30 * time.Minute // the code's own lifetime
)

var connectFileCodeRe = regexp.MustCompile(`^rysh_ct_[0-9a-f]{48}$`)

// connectFileBannedNames are field names refused anywhere in the file, so a
// key cannot ride along under a name the struct would merely ignore.
var connectFileBannedNames = []string{"api_key", "key", "secret", "token"}

type connectFile struct {
	Version int    `json:"rysh_connect"`
	Server  string `json:"server"`
	Code    string `json:"code"`
	Company *struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"company"`
	ExpiresAt string `json:"expires_at"`
}

// readConnectFile returns the server and code a connect file names, or an
// error that says why the file is refused.
func readConnectFile(path string, now time.Time) (server, code string, err error) {
	refuse := func(format string, a ...any) (string, string, error) {
		return "", "", fmt.Errorf("connect file %s: %s", path, fmt.Sprintf(format, a...))
	}
	st, err := os.Lstat(path)
	if err != nil {
		return refuse("%v", errors.Unwrap(err))
	}
	if !st.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	if now.Sub(st.ModTime()) > connectFileMaxAge {
		return refuse("older than %s; get a new one from the company page", connectFileMaxAge)
	}
	f, err := os.Open(path)
	if err != nil {
		return refuse("%v", errors.Unwrap(err))
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil || !os.SameFile(st, fst) {
		return refuse("changed while it was read")
	}
	if err := checkConnectFileOwner(fst); err != nil {
		return refuse("%v", err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, connectFileMaxSize+1))
	if err != nil {
		return refuse("%v", err)
	}
	if len(raw) > connectFileMaxSize {
		return refuse("larger than %d bytes", connectFileMaxSize)
	}
	if err := checkConnectFileTokens(raw); err != nil {
		return refuse("%v", err)
	}

	var cf connectFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return refuse("not a rysh connect file (only rysh_connect, server, code, company and expires_at may be in it)")
	}
	if cf.Version != connectFileVersion {
		return refuse("rysh_connect must be %d", connectFileVersion)
	}
	if !connectFileCodeRe.MatchString(cf.Code) {
		return refuse("code is not a connect code")
	}
	if err := checkConnectFileServer(cf.Server); err != nil {
		return refuse("%v", err)
	}
	if cf.ExpiresAt == "" {
		return refuse("expires_at is missing")
	}
	exp, err := time.Parse(time.RFC3339, cf.ExpiresAt)
	if err != nil {
		return refuse("expires_at is not an RFC 3339 time")
	}
	if !now.Before(exp) {
		return refuse("the code in it has expired; get a new one from the company page")
	}
	return cf.Server, cf.Code, nil
}

// checkConnectFileTokens walks the raw JSON token by token, so a duplicated
// key or a nested object cannot hide what the typed decode would drop: the top
// level must be one object, no field may carry a credential's name, and no
// string but the top-level code may look like a rysh secret (workspace keys
// are "rysh_" + random characters).
func checkConnectFileTokens(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	if err := walkConnectFileObject(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON object")
	}
	return nil
}

func walkConnectFileObject(dec *json.Decoder, depth int) error {
	if depth > 4 {
		return errors.New("nested too deep")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return errors.New("not valid JSON")
		}
		name, _ := tok.(string)
		if seen[name] {
			return fmt.Errorf("field %q appears twice", name)
		}
		seen[name] = true
		for _, banned := range connectFileBannedNames {
			if strings.EqualFold(name, banned) {
				return fmt.Errorf("it has a %q field; a connect file never carries a key", name)
			}
		}
		if err := walkConnectFileValue(dec, depth, depth == 0 && name == "code"); err != nil {
			return err
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return errors.New("not valid JSON")
	}
	return nil
}

func walkConnectFileValue(dec *json.Decoder, depth int, isCode bool) error {
	tok, err := dec.Token()
	if err != nil {
		return errors.New("not valid JSON")
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			return walkConnectFileObject(dec, depth+1)
		case '[':
			for dec.More() {
				if err := walkConnectFileValue(dec, depth+1, false); err != nil {
					return err
				}
			}
			if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
				return errors.New("not valid JSON")
			}
		}
	case string:
		if !isCode && strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "rysh_") {
			return errors.New("it carries a rysh secret besides the code; a connect file never carries a key")
		}
	}
	return nil
}

// checkConnectFileServer: the server is an origin — https, or http only to a
// loopback address — with no user, path, query or fragment.
func checkConnectFileServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return errors.New("server is not a URL")
	}
	switch {
	case u.User != nil:
		return errors.New("server carries a user name or password")
	case u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("server must be an origin only (no path, query or fragment)")
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	case u.Scheme == "http":
		return errors.New("server is plain http off this machine; use https")
	default:
		return errors.New("server must be https")
	}
	return nil
}
