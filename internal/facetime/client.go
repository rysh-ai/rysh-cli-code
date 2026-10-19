// SPDX-License-Identifier: Apache-2.0

// Package facetime provides local-only, user-confirmed macOS FaceTime handoff.
package facetime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

//go:embed assets/* native/Info.plist
var assets embed.FS

const Version = 1

type Request struct {
	Version       int    `json:"version"`
	Operation     string `json:"operation"`
	Search        string `json:"search,omitempty"`
	Offset        int    `json:"offset,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	ContactID     string `json:"contactID,omitempty"`
	AddressID     string `json:"addressID,omitempty"`
	ExpectedValue string `json:"expectedValue,omitempty"`
	ReceiveUID    string `json:"receiveUID,omitempty"`
	TransmitUID   string `json:"transmitUID,omitempty"`
	Seconds       int    `json:"seconds,omitempty"`
}
type Address struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}
type Contact struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Addresses []Address `json:"addresses"`
}
type AudioDevice struct {
	UID            string `json:"uid"`
	Name           string `json:"name"`
	InputChannels  int    `json:"inputChannels"`
	OutputChannels int    `json:"outputChannels"`
	Virtual        bool   `json:"virtual"`
}
type BotDoctor struct {
	OS                       string        `json:"os"`
	Microphone               string        `json:"microphone"`
	AccessibilityTrusted     bool          `json:"accessibilityTrusted"`
	Devices                  []AudioDevice `json:"devices"`
	IncomingCallsImplemented bool          `json:"incomingCallsImplemented"`
}
type AudioProbe struct {
	Seconds                  int     `json:"seconds"`
	ReceivedSamples          int     `json:"receivedSamples"`
	RMS                      float64 `json:"rms"`
	Peak                     float64 `json:"peak"`
	QueuedToneFrames         int     `json:"queuedToneFrames"`
	RemoteAudibilityVerified bool    `json:"remoteAudibilityVerified"`
}
type Reply struct {
	Bot        *BotDoctor  `json:"bot,omitempty"`
	Probe      *AudioProbe `json:"probe,omitempty"`
	Handler    string      `json:"handler,omitempty"`
	Version    int         `json:"version"`
	Permission string      `json:"permission,omitempty"`
	Available  bool        `json:"available"`
	Contacts   []Contact   `json:"contacts,omitempty"`
	NextOffset *int        `json:"nextOffset,omitempty"`
	Contact    *Contact    `json:"contact,omitempty"`
	Opened     bool        `json:"opened"`
	Error      string      `json:"error,omitempty"`
	Code       string      `json:"code,omitempty"`
}
type Client interface {
	Do(context.Context, Request) (Reply, error)
}
type NativeClient struct{}

func Validate(r Request) error {
	if r.Version != Version {
		return errors.New("incompatible FaceTime protocol version")
	}
	switch r.Operation {
	case "doctor", "contacts", "resolve", "call", "bot_doctor", "bot_probe":
	default:
		return errors.New("unknown FaceTime operation")
	}
	if len(r.Search) > 256 || r.Offset < 0 || r.Offset > 100000 || r.Limit < 0 || r.Limit > 200 {
		return errors.New("invalid contact search or page size")
	}
	if r.Operation == "bot_probe" {
		if r.ReceiveUID == "" || r.TransmitUID == "" || r.ReceiveUID == r.TransmitUID || len(r.ReceiveUID) > 1024 || len(r.TransmitUID) > 1024 || strings.IndexFunc(r.ReceiveUID+r.TransmitUID, unicode.IsControl) >= 0 || (r.Seconds != 0 && (r.Seconds < 5 || r.Seconds > 30)) {
			return errors.New("choose distinct receive/transmit device UIDs and a duration of 5–30 seconds")
		}
	}
	if r.Operation == "resolve" || r.Operation == "call" {
		if r.ContactID == "" || len(r.ContactID) > 1024 {
			return errors.New("choose a contact first")
		}
	}
	if r.Operation == "call" && (r.AddressID == "" || len(r.AddressID) > 1024 || r.ExpectedValue == "" || len(r.ExpectedValue) > 1024) {
		return errors.New("choose an address first")
	}
	return nil
}
func (NativeClient) Do(ctx context.Context, r Request) (Reply, error) {
	if err := Validate(r); err != nil {
		return Reply{}, err
	}
	if runtime.GOOS != "darwin" {
		return Reply{}, errors.New("FaceTime requires a locally signed-in macOS desktop")
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return Reply{}, errors.New("FaceTime is unavailable over SSH; run it on your local Mac")
	}
	path, err := helperPath()
	if err != nil {
		return Reply{}, err
	}
	return runNative(ctx, path, r)
}

func runNative(ctx context.Context, path string, r Request) (Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	body, _ := json.Marshal(r)
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = bytes.NewReader(body)
	// Never return native stderr: OS framework diagnostics can include contact data.
	var out boundedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Reply{}, errors.New("FaceTime request timed out or was canceled; retry when ready")
		}
		return Reply{}, errors.New("FaceTime helper failed; check macOS permissions and your Rysh installation")
	}
	var reply Reply
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil || reply.Version != Version {
		return Reply{}, errors.New("incompatible Contacts helper response; reinstall Rysh")
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

type boundedBuffer struct{ data bytes.Buffer }

func (b *boundedBuffer) Bytes() []byte { return b.data.Bytes() }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 4*1024*1024 {
		return 0, errors.New("response from the Contacts helper too large")
	}
	return b.data.Write(p)
}
func helperPath() (string, error) {
	binary, err := assets.ReadFile("assets/helper")
	if err != nil {
		return "", errors.New("this build has no Contacts helper; on macOS run scripts/build-facetime-helper.sh then rebuild Rysh")
	}
	plist, _ := assets.ReadFile("native/Info.plist")
	hash := fmt.Sprintf("%x", sha256.Sum256(append(append([]byte{}, binary...), plist...)))
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(cache, "rysh", "facetime", hash)
	path := filepath.Join(root, "Rysh Contacts.app", "Contents", "MacOS", "helper")
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, binary) {
		return path, nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	// Atomic replacement allows two clients to prepare the same helper safely.
	if err = atomicWrite(filepath.Join(root, "Rysh Contacts.app", "Contents", "Info.plist"), plist, 0600); err != nil {
		return "", err
	}
	if err = atomicWrite(path, binary, 0700); err != nil {
		return "", err
	}
	return path, nil
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".contacts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Bridge keeps all contact data on stdin/stdout, outside daemon logs and command history.
// Calling still ALWAYS requires the helper's native confirmation dialog.
func Bridge(ctx context.Context, c Client, in io.Reader, out io.Writer) error {
	var r Request
	dec := json.NewDecoder(io.LimitReader(in, 16385))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return errors.New("invalid FaceTime request")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("expected one FaceTime request")
	}
	if err := Validate(r); err != nil {
		return err
	}
	reply, err := c.Do(ctx, r)
	if err != nil {
		reply.Error = err.Error()
		if reply.Code == "" {
			reply.Code = "unavailable"
		}
	}
	reply.Version = Version
	return json.NewEncoder(out).Encode(reply)
}

// Display prevents contact text from injecting terminal control sequences.
func Display(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
