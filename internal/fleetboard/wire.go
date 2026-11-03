// SPDX-License-Identifier: Apache-2.0

package fleetboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ErrNoFleetBoard is returned by Ask when nothing answers.
var ErrNoFleetBoard = errors.New("fleet board: no answer from the session")

// ReplyRow is a row as served: the stored row plus its derived health and
// summary at the time of the answer.
type ReplyRow struct {
	Row
	Health  string `json:"health"`
	Summary string `json:"summary"`
}

// Reply answers msg.FleetBoardQuerySubject.
type Reply struct {
	Rows []ReplyRow `json:"rows"`
	Now  int64      `json:"now"`
	Err  string     `json:"err,omitempty"`
}

// MakeReply renders rows at now (unix millis).
func MakeReply(rows []Row, now int64) Reply {
	out := Reply{Rows: make([]ReplyRow, 0, len(rows)), Now: now}
	for _, r := range rows {
		out.Rows = append(out.Rows, ReplyRow{Row: r, Health: r.Health(now), Summary: r.Summary()})
	}
	return out
}

// Serve answers queries with rows(). The handler only reads a snapshot, so it
// never blocks on the actor's other work.
func Serve(nc *nats.Conn, rows func() []Row) (*nats.Subscription, error) {
	if nc == nil {
		return nil, errors.New("fleet board: no connection")
	}
	return nc.Subscribe(msg.FleetBoardQuerySubject(), func(m *nats.Msg) {
		data, err := json.Marshal(MakeReply(rows(), time.Now().UnixMilli()))
		if err != nil {
			data, _ = json.Marshal(Reply{Err: err.Error()})
		}
		_ = m.Respond(data)
	})
}

// Ask queries the session's fleet board.
func Ask(nc *nats.Conn, timeout time.Duration) (*Reply, error) {
	if nc == nil {
		return nil, fmt.Errorf("%w: no connection to the session bus", ErrNoFleetBoard)
	}
	m, err := nc.Request(msg.FleetBoardQuerySubject(), []byte("{}"), timeout)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoFleetBoard, err)
	}
	var reply Reply
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		return nil, fmt.Errorf("fleet board: unreadable answer: %w", err)
	}
	if reply.Err != "" {
		return nil, fmt.Errorf("fleet board: %s", reply.Err)
	}
	return &reply, nil
}
