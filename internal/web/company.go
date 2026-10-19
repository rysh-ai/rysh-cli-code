// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Company Connect (design 037): the desktop app paints the bound AI-native
// company from the same snapshot the terminal renders. The daemon's
// CompanyActor pushes it on company.snapshot and answers company.query; this
// file is the bridge to the /ws protocol — `company_snapshot` out,
// `company_query` and `company_refresh` in. See docs/ws-protocol.md §2.10 and
// §3.13.

const companyQueryTimeout = 3 * time.Second

func companySnapshotFrame(snap msg.CompanySnapshot) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"type": "company_snapshot",
		"data": map[string]interface{}{
			"snapshot": snap,
		},
	})
}

// subscribeCompanyEvents forwards every snapshot the follower publishes to all
// clients. A session not bound to a company publishes nothing here.
func (s *Server) subscribeCompanyEvents() {
	if sub, err := s.nc.Subscribe(msg.CompanySnapshotSubject(), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgCompanySnapshot)
		if !ok {
			return
		}
		data, err := companySnapshotFrame(r.Snapshot)
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to company snapshot", "err", err)
	} else {
		s.companySnapSub = sub
	}
}

// handleCompanyQuery answers company_query for the ORIGINATING client only —
// a window that just opened the company view must not wait for the next poll.
// A session with no follower never replies; the request times out quietly and
// the view keeps saying it is waiting, which is the truth.
func (s *Server) handleCompanyQuery(c *wsClient) {
	pub := s.pub
	go func() {
		res, err := pub.Request(msg.CompanyQuerySubject(), &msg.MsgCompanyQuery{}, companyQueryTimeout)
		if err != nil {
			return
		}
		r, ok := res.(*msg.MsgCompanySnapshot)
		if !ok {
			return
		}
		data, err := companySnapshotFrame(r.Snapshot)
		if err != nil {
			return
		}
		s.hub.sendWhere(data, func(x *wsClient) bool { return x == c })
	}()
}
