// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
)

// recallTransport is the Recall.ai backbone (design 040): a hosted bot
// created over Recall's API, signed transcript webhooks and an output-media
// webpage whose socket (Service.conn) carries reply audio. It reads the
// Service's Recall fields directly; the webhook, socket and OBF handlers stay
// on Service.ServeHTTP.
type recallTransport struct{ s *Service }

func (recallTransport) Kind() string { return "recall" }

func (t recallTransport) Join(ctx context.Context) (string, error) {
	s := t.s
	// Never retry this POST: an ambiguous response may already have created a bot.
	var created struct {
		ID string `json:"id"`
	}
	err := s.recall.call(ctx, "POST", "/bot/", withZoomOBF(createPayload(s.cfg, s.token), s.obf, s.cfg.PublicURL), &created)
	var rejected recallRejected
	if errors.As(err, &rejected) {
		// A 4xx is a definite refusal: no bot exists.
		return "", &joinError{definite: true, msg: rejected.Error() + "; no bot was created"}
	}
	if err != nil || !validBotID(created.ID) {
		return "", &joinError{msg: "Recall create not confirmed; inspect status and dashboard before retrying", notice: "Recall creation was not confirmed. Check Recall dashboard for an orphan bot before leave/reset; the POST is never retried."}
	}
	return created.ID, nil
}

func (t recallTransport) Leave(ctx context.Context) error {
	s := t.s
	s.mu.Lock()
	id := s.botID
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	err := s.recall.call(ctx, "POST", "/bot/"+id+"/leave_call/", nil, nil)
	var rejected recallRejected
	if errors.As(err, &rejected) && t.botEnded(ctx, id) {
		// Recall refuses commands to a bot that already left
		// (cannot_command_unstarted_bot); the goal of leave is met.
		err = nil
	}
	if err != nil {
		return &leaveError{msg: "Recall leave failed", notice: "Local execution and playback disabled; Recall may still transcribe. Leave failed; retry leave or remove the bot in Recall dashboard."}
	}
	return nil
}

// botEnded reports whether Recall's latest status for the bot is terminal.
func (t recallTransport) botEnded(ctx context.Context, id string) bool {
	code, _, err := t.lastCode(ctx, id)
	if err != nil {
		return false
	}
	switch code {
	case "done", "fatal", "call_ended":
		return true
	}
	return false
}

// lastCode is Recall's latest status code for the bot and its sub_code (why a
// bot ended: e.g. bot_kicked_from_waiting_room, timeout_exceeded_waiting_room,
// or a Zoom join refusal), "" when Recall gave none.
func (t recallTransport) lastCode(ctx context.Context, id string) (string, string, error) {
	var r struct {
		Status []struct {
			Code    string  `json:"code"`
			SubCode *string `json:"sub_code"`
		} `json:"status_changes"`
	}
	if err := t.s.recall.call(ctx, "GET", "/bot/"+id+"/", nil, &r); err != nil {
		return "", "", err
	}
	if len(r.Status) == 0 {
		return "", "", nil
	}
	last := r.Status[len(r.Status)-1]
	sub := ""
	if last.SubCode != nil && subCode.MatchString(*last.SubCode) {
		sub = *last.SubCode
	}
	return last.Code, sub, nil
}

// subCode bounds what of a provider's sub_code reaches status and the pane.
var subCode = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)

func (t recallTransport) Status(ctx context.Context) (TransportStatus, error) {
	t.s.mu.Lock()
	id, platform := t.s.botID, t.s.platformLocked()
	t.s.mu.Unlock()
	code, sub, err := t.lastCode(ctx, id)
	if err != nil {
		return TransportStatus{}, errors.New("Recall status unavailable; capture paused until connectivity recovers.")
	}
	switch code {
	case "":
		return TransportStatus{Unknown: true}, nil
	case "in_call_recording":
		return TransportStatus{Admitted: true}, nil
	case "done", "fatal", "call_ended":
		return TransportStatus{Ended: true, Why: sub}, nil
	}
	// Say what the bot is waiting for; Zoom and Teams add steps Meet lacks.
	st := TransportStatus{Lost: "Recall media permission/admission is not active", Waiting: recallWaiting(platform, code)}
	return st, nil
}

func (t recallTransport) MediaReadyLocked() bool { return t.s.conn != nil }

func (t recallTransport) SpeakLocked(turnID string, audio []byte) error {
	conn := t.s.conn
	if conn == nil {
		return errors.New("no output media socket")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	err := conn.WriteJSON(map[string]string{"type": "audio", "turn": turnID})
	if err == nil {
		err = conn.WriteMessage(websocket.BinaryMessage, audio)
	}
	if err != nil {
		_ = conn.Close()
	}
	return err
}

func (t recallTransport) ClearLocked() {
	if conn := t.s.conn; conn != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if conn.WriteJSON(map[string]string{"type": "clear"}) != nil {
			_ = conn.Close()
		}
	}
}
