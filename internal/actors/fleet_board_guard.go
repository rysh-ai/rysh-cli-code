// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/boardstamp"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Who may post to the reserved fleet board (design 030 §9.3, founder ruling
// D-24, wave W3; security-lead's notes out/e20-030-w3-plan-notes.md @ root
// 88d9a5726).
//
//   - THE DOORS (`rysh board post|reply|send` via MsgCLIBoardPost, `##board`)
//     ask the registry on every post and refuse unless the poster is a fleet
//     head (fleet.FleetBoardPoster). No registry answer = refused: fail
//     closed. An accepted post gets a boardstamp.Stamp.
//   - THE RECORDER (ABLA, and the FleetBoardActor's own read) tags a
//     fleet-board post Unverified unless its stamp is valid, fresh and unseen,
//     AND its pane is a head in the registry snapshot. It never drops a post,
//     and it clears the stamp before recording.
//
// NOT AUTHENTICATION ACROSS SESSIONS. The stamp key lives in this daemon only;
// a post relayed from elsewhere reads unverified. Within the recorder's
// 20-second registry snapshot a just-demoted head can still read verified; the
// doors ask fresh every time.

const (
	fleetBoardDoorTimeout   = 1500 * time.Millisecond
	fleetBoardHeadsTTL      = 20 * time.Second
	fleetBoardHeadsMissWait = 2 * time.Second
	fleetBoardStampWindow   = 5 * time.Minute
)

// guardFleetBoardPost is the doors' check. On the fleet board it refuses a
// non-head (or a silent registry) and stamps what it accepts; on any other
// board it clears both W3 fields (the recorder owns them) and accepts.
func (w *WorkspaceActor) guardFleetBoardPost(boardID string, post *msg.MsgBoardPost) error {
	post.Stamp, post.Unverified = "", false
	if msg.NormalizeBoardID(boardID) != msg.FleetBoardID {
		return nil
	}
	reply, err := fleet.Ask(w.nc, fleet.Query{}, fleetBoardDoorTimeout)
	if err != nil {
		slog.Warn("fleet board post refused", "board", msg.FleetBoardID, "pane", post.PaneID,
			"rule", "registry did not answer")
		return fmt.Errorf("the fleet registry did not answer, so whether %s (pane %s) is a fleet head cannot be "+
			"checked; nothing was posted to the fleet board (design 030, D-24)", post.Persona, post.PaneID)
	}
	if _, ok := fleet.FleetBoardPoster(reply.Fleets, post.PaneID); !ok {
		slog.Warn("fleet board post refused", "board", msg.FleetBoardID, "pane", post.PaneID,
			"rule", "not a fleet head")
		return fmt.Errorf("only a fleet head (a registered fleet member with role %s) may post to the fleet "+
			"board; %s (pane %s) is not one, so nothing was posted (design 030, D-24). Post to your fleet's "+
			"own board instead", strings.Join(fleet.TopRoles, ", "), post.Persona, post.PaneID)
	}
	post.Stamp = boardstamp.Stamp(boardID, post)
	return nil
}

// fleetHeadCache answers "is this pane a fleet head?" from a registry snapshot
// refreshed every fleetBoardHeadsTTL, and at most every
// fleetBoardHeadsMissWait on a miss.
type fleetHeadCache struct {
	nc       *nats.Conn
	mu       sync.Mutex
	fleets   []fleet.Fleet
	at       time.Time
	lastMiss time.Time
}

func (c *fleetHeadCache) refresh(now time.Time) {
	if reply, err := fleet.Ask(c.nc, fleet.Query{}, fleetBoardDoorTimeout); err == nil {
		c.fleets, c.at = reply.Fleets, now
	}
}

func (c *fleetHeadCache) isHead(pane string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.at) > fleetBoardHeadsTTL {
		c.refresh(now)
	}
	if _, ok := fleet.FleetBoardPoster(c.fleets, pane); ok {
		return true
	}
	if now.Sub(c.lastMiss) < fleetBoardHeadsMissWait {
		return false
	}
	c.lastMiss = now
	c.refresh(now)
	_, ok := fleet.FleetBoardPoster(c.fleets, pane)
	return ok
}

// fleetBoardTagger is the recorder's board.TagFunc.
type fleetBoardTagger struct {
	heads *fleetHeadCache
	guard *boardstamp.ReplayGuard
}

func newFleetBoardTagger(nc *nats.Conn) *fleetBoardTagger {
	return &fleetBoardTagger{heads: &fleetHeadCache{nc: nc}, guard: boardstamp.NewReplayGuard(fleetBoardStampWindow)}
}

// tag sets Unverified on a fleet-board post that lacks a valid, fresh, unseen
// stamp from a head, and clears the stamp; on other boards it clears both.
// The stamp is verified before the replay guard sees it, so forged stamps
// cannot fill the guard.
func (t *fleetBoardTagger) tag(boardID string, p *msg.MsgBoardPost) {
	if p == nil {
		return
	}
	if msg.NormalizeBoardID(boardID) != msg.FleetBoardID {
		p.Stamp, p.Unverified = "", false
		return
	}
	ok := boardstamp.Verify(boardID, p) && t.guard.Accept(p, time.Now()) && t.heads.isHead(p.PaneID)
	p.Unverified = !ok
	p.Stamp = ""
}
