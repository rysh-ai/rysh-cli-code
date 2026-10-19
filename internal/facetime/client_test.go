// SPDX-License-Identifier: Apache-2.0

package facetime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeClient struct {
	calls []Request
	reply Reply
	err   error
}

func (f *fakeClient) Do(_ context.Context, r Request) (Reply, error) {
	f.calls = append(f.calls, r)
	return f.reply, f.err
}
func TestBridgeRejectsMalformedRequestsWithoutCalling(t *testing.T) {
	for _, input := range []string{`{}`, `{"version":2,"operation":"doctor"}`, `{"version":1,"operation":"contacts","offset":-1}`, `{"version":1,"operation":"call","contactID":"x"}`, `{"version":1,"operation":"doctor","shell":"bad"}`, `{"version":1,"operation":"doctor"} {}`} {
		f := &fakeClient{}
		var out bytes.Buffer
		if err := Bridge(context.Background(), f, strings.NewReader(input), &out); err == nil {
			t.Errorf("accepted %s", input)
		}
		if len(f.calls) != 0 {
			t.Fatal("invalid input reached helper")
		}
	}
}
func TestBridgePermissionFailureAndPage(t *testing.T) {
	f := &fakeClient{err: errors.New("permission denied"), reply: Reply{Code: "permission_denied"}}
	var out bytes.Buffer
	if err := Bridge(context.Background(), f, strings.NewReader(`{"version":1,"operation":"contacts","search":"Zoë","limit":50}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"code":"permission_denied"`) || !strings.Contains(out.String(), `"version":1`) {
		t.Fatal(out.String())
	}
	if f.calls[0].Search != "Zoë" {
		t.Fatal(f.calls)
	}
}
func TestDisplayStripsTerminalAndBidiControls(t *testing.T) {
	got := Display("Zoë\x1b[2J\n\u202eAlice")
	if strings.ContainsAny(got, "\x1b\n\u202e") {
		t.Fatal(got)
	}
}
func TestPickerNeverCallsFromListingAndCallsOnlySelectedAddress(t *testing.T) {
	f := &fakeClient{reply: Reply{Contacts: []Contact{{ID: "a", Name: "Alex", Addresses: []Address{{ID: "email", Value: "a@example.invalid"}, {ID: "phone", Value: "+15555550100"}}}, {ID: "b", Name: "Alex"}}}}
	model := NewPicker(context.Background(), f)
	model, _ = model.Update(model.Init()())
	key := func(k tea.KeyType) {
		t.Helper()
		var cmd tea.Cmd
		model, cmd = model.Update(tea.KeyMsg{Type: k})
		if cmd != nil {
			model, _ = model.Update(cmd())
		}
	}
	key(tea.KeyEnter) // choose contact, not a call
	if len(f.calls) != 1 {
		t.Fatal("contact selection launched a call")
	}
	key(tea.KeyDown)
	key(tea.KeyEnter)
	if len(f.calls) != 2 || f.calls[1].Operation != "call" || f.calls[1].AddressID != "phone" || f.calls[1].ExpectedValue != "+15555550100" {
		t.Fatal(f.calls)
	}
}
func TestPickerNoAddressAndBusyDoubleEnter(t *testing.T) {
	p := picker{client: &fakeClient{}, ctx: context.Background(), selected: &Contact{ID: "a"}}
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("empty contact launched")
	}
	p.selected.Addresses = []Address{{ID: "a", Value: "a@example.invalid"}}
	m, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("call not reachable")
	}
	_, again := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if again != nil {
		t.Fatal("double call")
	}
}
func TestPickerPermissionFailureClearsContactData(t *testing.T) {
	p := picker{contacts: []Contact{{ID: "a"}}, selected: &Contact{ID: "a"}}
	m, _ := p.Update(resultMsg{err: errors.New("permission denied")})
	if len(m.(picker).contacts) != 0 || m.(picker).selected != nil {
		t.Fatal("kept contacts after revocation")
	}
}
func TestPickerCancelDoesNotCall(t *testing.T) {
	f := &fakeClient{}
	p := picker{client: f, selected: &Contact{ID: "a"}}
	m, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || m.(picker).selected != nil || len(f.calls) != 0 {
		t.Fatal("cancel had side effects")
	}
}

func TestNativeResponseVersionTimeoutAndBound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '{\"version\":2}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runNative(context.Background(), path, Request{Version: 1, Operation: "doctor"}); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := runNative(ctx, path, Request{Version: 1, Operation: "doctor"}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatal(err)
	}
	var out boundedBuffer
	if _, err := io.Copy(&out, strings.NewReader(strings.Repeat("x", 4*1024*1024+1))); err == nil {
		t.Fatal("output cap bypassed via io.Copy")
	}
}

func TestBotBridgeValidationAndDiagnostic(t *testing.T) {
	for _, input := range []string{
		`{"version":1,"operation":"bot_probe","receiveUID":"rx","transmitUID":"rx"}`,
		`{"version":1,"operation":"bot_probe","receiveUID":"rx","transmitUID":"tx","seconds":31}`,
		`{"version":1,"operation":"bot_probe","receiveUID":"rx","transmitUID":"tx\n"}`,
	} {
		f := &fakeClient{}
		if err := Bridge(context.Background(), f, strings.NewReader(input), &bytes.Buffer{}); err == nil || len(f.calls) != 0 {
			t.Fatalf("invalid probe reached native helper: %s", input)
		}
	}
	f := &fakeClient{reply: Reply{Bot: &BotDoctor{Microphone: "not_determined", Devices: []AudioDevice{}}}}
	var out bytes.Buffer
	if err := Bridge(context.Background(), f, strings.NewReader(`{"version":1,"operation":"bot_doctor"}`), &out); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].Operation != "bot_doctor" || !strings.Contains(out.String(), `"incomingCallsImplemented":false`) {
		t.Fatal(out.String(), f.calls)
	}
}
