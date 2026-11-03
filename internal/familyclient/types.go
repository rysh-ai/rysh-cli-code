// SPDX-License-Identifier: Apache-2.0

// Package familyclient is the terminal's client for rysh-family (design 093):
// a typed REST client for rysh-server's /api/family routes, a socket client
// that follows one family's event stream (with reconnect), name/prefix
// resolution, and the plain-text formatting `rysh family …` prints.
//
// The wire contract is rysh-family docs/API.md (v1, MVP). Every type here
// mirrors a type there field for field; unknown fields are ignored so a newer
// server does not break an older CLI.
package familyclient

import (
	"encoding/json"
	"strconv"
	"time"
)

// Avatar is one member of a family (API.md `Avatar`).
type Avatar struct {
	ID        string   `json:"id"`
	FamilyID  string   `json:"family_id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Persona   string   `json:"persona"`
	Mascot    string   `json:"mascot"`
	SeatIndex int      `json:"seat_index"`
	Status    string   `json:"status"`
	Pct       *float64 `json:"pct"`
	Note      string   `json:"note"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// Family is one family with its avatars (API.md `Family`).
type Family struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Theme     string   `json:"theme"`
	CreatedAt string   `json:"created_at"`
	Avatars   []Avatar `json:"avatars"`
}

// Message is one stored chat message (API.md `Message`).
type Message struct {
	ID           string  `json:"id"`
	FamilyID     string  `json:"family_id"`
	AvatarID     string  `json:"avatar_id"`
	Direction    string  `json:"direction"` // user | avatar | avatar_to_avatar | system
	FromAvatarID *string `json:"from_avatar_id"`
	ToAvatarID   *string `json:"to_avatar_id"`
	Body         string  `json:"body"`
	CreatedAt    string  `json:"created_at"`
}

// Task is one task an avatar keeps (API.md `Task`).
type Task struct {
	ID        string   `json:"id"`
	FamilyID  string   `json:"family_id"`
	AvatarID  string   `json:"avatar_id"`
	Title     string   `json:"title"`
	State     string   `json:"state"` // open | doing | done
	Pct       *float64 `json:"pct"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// Tokens is the /api/auth/login and /api/auth/refresh response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Event is one socket frame (API.md "Socket events"). Only the fields of the
// frame's Type are set.
type Event struct {
	Type         string   `json:"type"`
	Family       *Family  `json:"family,omitempty"`
	Message      *Message `json:"-"`
	AvatarID     string   `json:"avatar_id,omitempty"`
	ReplyID      string   `json:"reply_id,omitempty"`
	Text         string   `json:"text,omitempty"`
	Status       string   `json:"status,omitempty"`
	Pct          *float64 `json:"pct,omitempty"`
	Note         string   `json:"note,omitempty"`
	FromAvatarID string   `json:"from_avatar_id,omitempty"`
	ToAvatarID   string   `json:"to_avatar_id,omitempty"`
	Task         *Task    `json:"task,omitempty"`
	Avatar       *Avatar  `json:"avatar,omitempty"`

	// ErrMessage is the `message` of an `error` frame. The wire reuses the
	// key `message` (a string there, an object on `message`/`reply_done`), so
	// both are decoded by UnmarshalJSON rather than by a tag.
	ErrMessage string `json:"-"`

	// Received is when this client read the frame (not a wire field).
	Received time.Time `json:"-"`
}

// UnmarshalJSON decodes a frame, splitting the overloaded `message` key.
func (e *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var m struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*e = Event(p)
	if raw := m.Message; len(raw) > 0 && string(raw) != "null" {
		if raw[0] == '"' {
			if err := json.Unmarshal(raw, &e.ErrMessage); err != nil {
				return err
			}
		} else {
			var msg Message
			if err := json.Unmarshal(raw, &msg); err != nil {
				return err
			}
			e.Message = &msg
		}
	}
	return nil
}

// MarshalJSON re-encodes a frame with the overloaded `message` key (used by
// `rysh family watch --json` and by tests' fake servers).
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	b, err := json.Marshal(plain(e))
	if err != nil {
		return nil, err
	}
	var extra any
	switch {
	case e.Message != nil:
		extra = e.Message
	case e.ErrMessage != "":
		extra = e.ErrMessage
	default:
		return b, nil
	}
	mb, err := json.Marshal(extra)
	if err != nil {
		return nil, err
	}
	// b is a JSON object ending in '}'; splice "message" in.
	out := append([]byte{}, b[:len(b)-1]...)
	if len(b) > 2 {
		out = append(out, ',')
	}
	out = append(out, `"message":`...)
	out = append(out, mb...)
	return append(out, '}'), nil
}

// APIError is a non-2xx answer from the server ({error, message?}).
type APIError struct {
	Status  int
	Code    string // the `error` field
	Message string // the `message` field
}

func (e *APIError) Error() string {
	s := e.Code
	if s == "" {
		s = "request failed"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	switch e.Status {
	case 401:
		return "not signed in, or the session expired (HTTP 401: " + s + "); run `rysh family login`"
	case 402:
		return "a family subscription is required (HTTP 402: " + s + ")"
	case 404:
		return "not found (HTTP 404: " + s + ")"
	}
	return s + " (HTTP " + strconv.Itoa(e.Status) + ")"
}
