// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"github.com/rysh-ai/rysh-cli-code/internal/facetime"
	"strings"
	"testing"
)

type faceTimeFake struct{ calls []facetime.Request }

func (f *faceTimeFake) Do(_ context.Context, r facetime.Request) (facetime.Reply, error) {
	f.calls = append(f.calls, r)
	if r.Operation == "resolve" {
		return facetime.Reply{Contact: &facetime.Contact{ID: "c", Addresses: []facetime.Address{{ID: "a", Value: "alice@example.invalid"}}}}, nil
	}
	return facetime.Reply{Version: 1, Contacts: []facetime.Contact{{ID: "c", Name: "Alice"}}}, nil
}
func TestFaceTimeCommandRoutingAndNoNoninteractiveCall(t *testing.T) {
	for _, args := range [][]string{{"call", "--contact", "c", "--address", "a"}, {"contacts", "--limit", "0"}, {"doctor", "extra"}, {"contacts", "--bad"}} {
		f := &faceTimeFake{}
		err := faceTimeCommand(context.Background(), f, args, &bytes.Buffer{}, false)
		if err == nil || len(f.calls) != 0 {
			t.Fatalf("%v: err=%v calls=%v", args, err, f.calls)
		}
	}
}
func TestFaceTimeCallResolvesAddressBeforeHandoff(t *testing.T) {
	f := &faceTimeFake{}
	var out bytes.Buffer
	err := faceTimeCommand(context.Background(), f, []string{"call", "--contact", "c", "--address", "a"}, &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[1].ExpectedValue != "alice@example.invalid" {
		t.Fatal(f.calls)
	}
	if out.String() != "Call canceled.\n" {
		t.Fatal(out.String())
	}
}
func TestFaceTimeContactsJSONAndDoctor(t *testing.T) {
	f := &faceTimeFake{}
	var out bytes.Buffer
	if err := faceTimeCommand(context.Background(), f, []string{"contacts", "--search", "Zoë", "--json", "--offset", "50"}, &out, false); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].Search != "Zoë" || f.calls[0].Offset != 50 || !strings.Contains(out.String(), `"name":"Alice"`) {
		t.Fatal(out.String(), f.calls)
	}
	f = &faceTimeFake{}
	out.Reset()
	if err := faceTimeCommand(context.Background(), f, []string{"doctor"}, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].Operation != "doctor" {
		t.Fatal("doctor accessed contacts")
	}
}

type botFake struct {
	calls []facetime.Request
	reply facetime.Reply
}

func (f *botFake) Do(_ context.Context, r facetime.Request) (facetime.Reply, error) {
	f.calls = append(f.calls, r)
	return f.reply, nil
}
func TestBotDoctorDoesNotCaptureOrClaimReady(t *testing.T) {
	f := &botFake{reply: facetime.Reply{Bot: &facetime.BotDoctor{OS: "fixture", Microphone: "not_determined", Devices: []facetime.AudioDevice{{Name: "fake\x1bdevice", UID: "rx", Virtual: true}}}}}
	var out bytes.Buffer
	if err := faceTimeCommand(context.Background(), f, []string{"bot", "doctor"}, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].Operation != "bot_doctor" || f.calls[0].ReceiveUID != "" {
		t.Fatal(f.calls)
	}
	if !strings.Contains(out.String(), "not implemented") || strings.Contains(out.String(), "\x1b") {
		t.Fatal(out.String())
	}
}
func TestBotProbeRejectsInvalidArgumentsBeforeNative(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		interactive bool
	}{
		{[]string{"bot", "probe", "--receive", "rx", "--transmit", "tx"}, false},
		{[]string{"bot", "probe", "--receive", "rx", "--transmit", "rx"}, true},
		{[]string{"bot", "probe", "--receive", "rx"}, true},
		{[]string{"bot", "probe", "--receive", "rx", "--transmit", "tx", "--seconds", "0"}, true},
		{[]string{"bot", "probe", "--receive", "rx", "--transmit", "tx", "--seconds", "31"}, true},
		{[]string{"bot", "doctor", "--receive", "rx"}, true},
		{[]string{"bot", "start"}, true},
	} {
		f := &botFake{}
		if err := faceTimeCommand(context.Background(), f, tc.args, &bytes.Buffer{}, tc.interactive); err == nil || len(f.calls) != 0 {
			t.Fatalf("args=%v err=%v calls=%v", tc.args, err, f.calls)
		}
	}
}
func TestBotProbeMeasurementsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		reply facetime.Reply
		want  string
	}{
		{facetime.Reply{Code: "canceled"}, "Audio probe canceled"},
		{facetime.Reply{Probe: &facetime.AudioProbe{Seconds: 10, ReceivedSamples: 480000, Peak: 0.2, RMS: 0.1}}, "not proof of a working FaceTime bot"},
	} {
		f := &botFake{reply: tc.reply}
		var out bytes.Buffer
		if err := faceTimeCommand(context.Background(), f, []string{"bot", "probe", "--receive", "rx", "--transmit", "tx"}, &out, true); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) || len(f.calls) != 1 || f.calls[0].Operation != "bot_probe" || f.calls[0].Seconds != 10 {
			t.Fatal(out.String(), f.calls)
		}
	}
	f := &botFake{}
	if err := faceTimeCommand(context.Background(), f, []string{"bot", "doctor"}, &bytes.Buffer{}, false); err == nil {
		t.Fatal("accepted old helper without diagnostic")
	}
}
