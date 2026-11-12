// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// Steer (boat gap #6 / rr-order-154 C6): add a message to a pane's running
// turn without stopping it.
//
//   - rysh's own agent: a Queue + Literal prompt. It waits behind the run in
//     flight (never cancels it, QA-36) and no prefix of it is read as a
//     directive.
//   - a harness (claude, codex, ...) owns the PTY and never sees rysh's
//     prompt messages (F-25): the text is typed in through the ANSA router,
//     which is how a person adds a message while the harness works.

// steerPane steers one pane. deliver types text into a pane running a
// program. It returns how the message went in.
func steerPane(c paneBus, deliver func(paneID, text string) error, paneID, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("steer: the text is empty; nothing was sent")
	}
	reply, err := c.RequestFromSubject(msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true}, sendPaneProbeTimeout)
	if err != nil {
		return "", fmt.Errorf("pane %s did not answer (a full pane id is needed; the pane may have exited); nothing was sent: %w", paneID, err)
	}
	program := ""
	if snap, ok := reply.(*msg.MsgPaneSnapshotReply); ok {
		program = strings.TrimSpace(snap.Snapshot.Program)
	}
	if program != "" {
		if deliver == nil {
			return "", errors.New("steer: no way to type into the pane; nothing was sent")
		}
		if err := deliver(paneID, text); err != nil {
			return "", fmt.Errorf("steer: typing into %s failed: %w", program, err)
		}
		return "typed into " + program + " (taken as a message during its turn)", nil
	}
	if err := c.SendToSubject(msg.T("pane", paneID, "inbox"), &msg.MsgPaneExecPrompt{Prompt: text, Literal: true, Queue: true}); err != nil {
		return "", fmt.Errorf("steer: %w", err)
	}
	return "queued behind the running turn (it is not stopped)", nil
}

// PaneSteer steers paneID of sessionName.
func PaneSteer(store *session.Store, sessionName, paneID, text string) (string, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return "", err
	}
	defer c.Close()
	deliver := func(p, t string) error {
		resp, err := AnsaSend(store, sessionName, "", p, msg.AnsaModeShell, t)
		if err != nil {
			return err
		}
		if !resp.OK {
			if resp.Error != "" {
				return errors.New(resp.Error)
			}
			return errors.New("ansa route failed")
		}
		return nil
	}
	return steerPane(c, deliver, paneID, text)
}
