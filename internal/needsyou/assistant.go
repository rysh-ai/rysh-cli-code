// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Requester is the part of a session client the assistant sink uses.
type Requester interface {
	RequestFromSubject(subject string, m interface{}, timeout time.Duration) (interface{}, error)
}

// assistantTimeout bounds one request to the humanoid.
const assistantTimeout = 10 * time.Second

// AssistantSink asks a humanoid to tell its owner, over the owner's own DM
// with the bot (E33 slice E1, design 086). For a user whose only remote
// surface is the bot `rysh setup` gave them, this is how a pane waiting on
// an approval stops being silent. The humanoid decides where it lands and
// refuses when it has no owner DM; this sink only reports what it answered.
type AssistantSink struct {
	c        Requester
	humanoid string
}

// NewAssistantSink returns a sink for the named humanoid.
func NewAssistantSink(c Requester, humanoid string) *AssistantSink {
	return &AssistantSink{c: c, humanoid: humanoid}
}

// Name implements Sink.
func (s *AssistantSink) Name() string { return "assistant" }

// Send implements Sink.
func (s *AssistantSink) Send(_ context.Context, a Alert) error {
	reply, err := s.c.RequestFromSubject(msg.T("humanoid", s.humanoid, "inbox"),
		&msg.MsgHumanoidNotifyOwner{Text: a.Text()}, assistantTimeout)
	if err != nil {
		return fmt.Errorf("assistant: humanoid %s did not answer: %w", s.humanoid, err)
	}
	r, ok := reply.(*msg.MsgHumanoidNotifyOwnerReply)
	if !ok {
		return fmt.Errorf("assistant: unexpected reply type %T", reply)
	}
	if r.Outcome == msg.NotifyOwnerSent {
		return nil
	}
	if r.Error != "" {
		return errors.New("assistant: " + r.Error)
	}
	return fmt.Errorf("assistant: not sent (%s)", r.Outcome)
}
