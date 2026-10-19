// SPDX-License-Identifier: Apache-2.0

package actors

// The status scout's actor side (design 056 §3): gather the evidence, ask the
// cheap model, declare for the agent — or fall back to asking the agent. The
// decisions (who is eligible, how a reply becomes a declaration) are pure, in
// internal/fleet/scout.go.

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/courier"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

var (
	// scoutCall is the model call; a variable so a test can stand in for it.
	scoutCall = provider.ClaudeOneShot
	// scoutTimeout bounds one scout answer; past it, the agent is asked.
	scoutTimeout = 2 * time.Minute
	// scoutParallel bounds concurrent scout calls across every fleet.
	scoutParallel = 3
)

// SetScoutCommand sets the claude CLI the scout runs (the session's
// configured claude command; "claude" on PATH when empty).
func (a *AgentFleetActor) SetScoutCommand(cmd string) {
	a.mu.Lock()
	a.scoutCmd = cmd
	a.mu.Unlock()
}

// tryScout starts a scout for an eligible check-in and reports whether it
// did. False means: send the check-in to the agent as usual.
func (a *AgentFleetActor) tryScout(pub *msg.NATSPublisher, c fleet.Checkin, w fleet.Watch, now int64) bool {
	if !fleet.ScoutEligible(c, w) {
		return false
	}
	a.mu.Lock()
	if a.scoutBusy == nil {
		a.scoutBusy = map[string]bool{}
		a.scoutSem = make(chan struct{}, scoutParallel)
	}
	if a.scoutBusy[c.Member.PaneID] {
		a.mu.Unlock()
		return true // one in flight already answers this member
	}
	a.scoutBusy[c.Member.PaneID] = true
	cmd, sem := a.scoutCmd, a.scoutSem
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.scoutBusy, c.Member.PaneID)
			a.mu.Unlock()
		}()
		sem <- struct{}{}
		defer func() { <-sem }()
		a.runScout(pub, c, w.ScoutModel, cmd, now)
	}()
	return true
}

// runScout answers one check-in with the scout, or falls back to the agent.
func (a *AgentFleetActor) runScout(pub *msg.NATSPublisher, c fleet.Checkin, model, cmd string, now int64) {
	f := a.reg.Get(c.Fleet)
	if f == nil {
		return
	}
	m := memberByPane(f, c.Member.PaneID)
	if m == nil {
		return
	}
	fallback := func(why string) {
		slog.Info("fleet scout: asking the agent instead", "fleet", f.Name, "member", m.Label, "why", why)
		if id := a.sendCheckin(pub, c, time.Now().UnixMilli()); id != "" {
			a.reg.CheckinSent(c.Fleet, m.PaneID, id)
		}
	}

	var screen []string
	if s, err := (&natsAnsaTransport{pub: pub}).Screen(m.PaneID); err == nil {
		screen = s
	}
	tool := false
	if t, err := courier.SnapshotVia(pub, m.PaneID); err == nil {
		tool = readProcTable().toolRunning(t.PGID)
	}
	posts := memberPosts(a, f.BoardID, m.PaneID, now)

	prompt := fleet.BuildScoutPrompt(f, m, c.Reason, screen, posts, tool, now)
	ctx, cancel := context.WithTimeout(context.Background(), scoutTimeout)
	defer cancel()
	text, err := scoutCall(ctx, cmd, model, fleet.ScoutSystemPrompt, prompt)
	if err != nil {
		fallback("scout call failed: " + err.Error())
		return
	}
	ans, err := fleet.ParseScoutAnswer(text)
	if err != nil {
		fallback(err.Error())
		return
	}
	u, err := fleet.ScoutUpdate(ans, model)
	if err != nil {
		fallback(err.Error())
		return
	}
	health := ""
	if m.Status != nil {
		health = m.Status.Health
	}
	res, err := a.reg.Declare(f.Name, m.PaneID, u, time.Now().UnixMilli())
	if err != nil {
		fallback("the ledger refused the scout's answer: " + err.Error())
		return
	}
	a.reg.ScoutAnswered(f.Name, m.PaneID, health, time.Now().UnixMilli())
	a.onStatus(fleet.Update{Op: fleet.OpStatus, Name: f.Name, PaneID: m.PaneID},
		fleet.UpdateReply{OK: true, Fleet: a.reg.Get(f.Name), Member: res.Member})
	a.mu.Lock()
	persist := a.persist
	a.mu.Unlock()
	if persist != nil {
		if cur := a.reg.Get(f.Name); cur != nil {
			_ = persist.Save(cur)
		}
	}
}

// memberPosts returns the member's last few board posts, oldest first.
func memberPosts(a *AgentFleetActor, boardID, paneID string, now int64) []string {
	reply, err := board.Ask(a.nc, boardID, board.Query{Since: now - 2*time.Hour.Milliseconds()}, 2*time.Second)
	if err != nil {
		return nil
	}
	type tp struct {
		ts   int64
		text string
	}
	var out []tp
	for _, t := range reply.Threads {
		for _, p := range append([]*msg.MsgBoardPost{t.Root}, t.Replies...) {
			if p != nil && p.PaneID == paneID {
				out = append(out, tp{p.TS, p.Kind + ": " + p.Text})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	if len(out) > 6 {
		out = out[len(out)-6:]
	}
	texts := make([]string, len(out))
	for i, x := range out {
		texts[i] = x.text
	}
	return texts
}
