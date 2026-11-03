// SPDX-License-Identifier: Apache-2.0

package needsyou

// E33 slice E1 (design 086): the assistant sink asks a humanoid to tell its
// owner, over the owner's DM with the bot, that a pane needs them.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

type fakeRequester struct {
	subject string
	req     interface{}
	reply   interface{}
	err     error
}

func (f *fakeRequester) RequestFromSubject(subject string, m interface{}, _ time.Duration) (interface{}, error) {
	f.subject, f.req = subject, m
	return f.reply, f.err
}

var ownerAlert = Alert{Session: "work", Host: "mac", PaneID: "p1", PaneName: "claude-1", Reason: "approval", Since: 1}

func TestAssistantSinkSendsTheAlertTextToTheHumanoid(t *testing.T) {
	f := &fakeRequester{reply: &msg.MsgHumanoidNotifyOwnerReply{Outcome: msg.NotifyOwnerSent}}
	s := NewAssistantSink(f, "assistant")
	if s.Name() != "assistant" {
		t.Fatalf("name = %q", s.Name())
	}
	if err := s.Send(context.Background(), ownerAlert); err != nil {
		t.Fatalf("send: %v", err)
	}
	if f.subject != msg.T("humanoid", "assistant", "inbox") {
		t.Fatalf("subject = %q", f.subject)
	}
	req, ok := f.req.(*msg.MsgHumanoidNotifyOwner)
	if !ok || req.Text != ownerAlert.Text() {
		t.Fatalf("request = %#v, want the alert's text", f.req)
	}
}

func TestAssistantSinkReportsEveryFailure(t *testing.T) {
	cases := []struct {
		name  string
		reply interface{}
		err   error
		want  string
	}{
		{name: "no owner chat", reply: &msg.MsgHumanoidNotifyOwnerReply{Outcome: msg.NotifyOwnerNoChat, Error: "send the bot one direct message"}, want: "direct message"},
		{name: "humanoid error", reply: &msg.MsgHumanoidNotifyOwnerReply{Error: "send failed"}, want: "send failed"},
		{name: "no answer", err: errors.New("nats: timeout"), want: "did not answer"},
		{name: "wrong reply type", reply: &msg.MsgCLIResponse{}, want: "unexpected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewAssistantSink(&fakeRequester{reply: c.reply, err: c.err}, "assistant")
			err := s.Send(context.Background(), ownerAlert)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
