// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	coord "github.com/rysh-ai/rysh-cli-code/internal/peercoord"
)

type peerHostRequest struct{ Request coord.HostRequest }

func (f *WorkspaceFarmActor) startPeerRuntime(ctx actor.Context) {
	root := ctx.ActorSystem().Root
	pid := ctx.Self()
	f.peerRuntime = coord.New(f.cfg.RyshDir, f.sessionName, func(c context.Context, q coord.HostRequest) coord.HostResponse {
		timeout := 3 * time.Second
		if d, ok := c.Deadline(); ok && time.Until(d) < timeout {
			timeout = time.Until(d)
		}
		if timeout <= 0 {
			return coord.HostResponse{Err: c.Err()}
		}
		reply, e := root.RequestFuture(pid, &peerHostRequest{Request: q}, timeout).Result()
		if e != nil {
			return coord.HostResponse{Err: e}
		}
		out, ok := reply.(coord.HostResponse)
		if !ok {
			return coord.HostResponse{Err: fmt.Errorf("invalid peer host response")}
		}
		return out
	})

	if e := f.peerRuntime.Start(); e != nil {
		fmt.Printf("peer coordination listener: %v\n", e)
	}
}
func (w *WorkspaceActor) peerHost(q coord.HostRequest) coord.HostResponse {
	switch q.Op {
	case "peer-command":
		response := w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{AsPaneID: q.Pane, Args: q.Args})
		if !response.OK {
			return coord.HostResponse{Err: fmt.Errorf("%s", response.Error)}
		}
		return coord.HostResponse{Value: response.Output}
	case "secret":
		return coord.HostResponse{Value: w.peerKey(q.Secret)}
	case "inspect":
		p := w.paneSnapshotByID(q.Pane)
		if p == nil {
			return coord.HostResponse{}
		}
		if q.Execution != "" && p.Meta["peer.execution"] != q.Execution {
			return coord.HostResponse{Err: fmt.Errorf("pane execution identity mismatch")}
		}
		return coord.HostResponse{Exists: true}
	case "create":
		if p := w.paneSnapshotByID(q.Pane); p != nil {
			if p.Meta["peer.execution"] != q.Execution {
				return coord.HostResponse{Err: fmt.Errorf("pane UUID collision")}
			}
			return coord.HostResponse{Exists: true}
		}
		origin := w.paneSnapshotByID(q.Origin)
		if origin == nil {
			return coord.HostResponse{Err: fmt.Errorf("registration pane is unavailable; rebind locally")}
		}
		tab := w.resolveOriginTab(q.Origin)
		if tab == nil {
			return coord.HostResponse{Err: fmt.Errorf("origin tab unavailable")}
		}
		lane := origin.LaneID
		if lane == "" {
			lane = w.resolveLaneInTab(tab, "")
		}
		if e := w.checkLimits(1); e != nil {
			return coord.HostResponse{Err: e}
		}
		e := w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePaneGroupInLane{LaneID: lane, GroupID: uuid.NewString(), PaneID: q.Pane, Title: w.generateUniqueAlias(), WorkingDir: q.Directory, Meta: map[string]string{"peer.execution": q.Execution, "peer.endpoint": q.Name}})
		if e != nil {
			return coord.HostResponse{Err: e}
		}
		w.resCounts.panes++
		w.restoreFocusAfterCreate(q.Origin)
		w.persistToKV()
		return coord.HostResponse{Exists: true}
	case "focus":
		p := w.paneSnapshotByID(q.Pane)
		if p == nil || p.Meta["peer.execution"] != q.Execution {
			return coord.HostResponse{Err: fmt.Errorf("local task pane unavailable")}
		}
		w.focusPaneByID(q.Pane)
		return coord.HostResponse{Exists: true}
	case "launch":
		p := w.paneSnapshotByID(q.Pane)
		if p == nil || p.Meta["peer.execution"] != q.Execution {
			return coord.HostResponse{Err: fmt.Errorf("owned peer pane unavailable")}
		}
		// Only a fresh owned pane receives a shell command. Subsequent turns use
		// the supervisor's structured provider API, never pane keystrokes.
		if p.Meta["peer.launched"] != "" {
			return coord.HostResponse{Err: fmt.Errorf("pane launch already issued")}
		}
		_ = w.pub.Send(msg.T("pane", q.Pane, "inbox"), &msg.MsgPaneSetMeta{Key: "peer.launched", Value: "true"})
		_ = w.pub.Send(msg.T("pane", q.Pane, "inbox"), &msg.MsgPaneNativeMode{Action: "on"})
		e := w.pub.Send(msg.T("pane", q.Pane, "inbox"), &msg.MsgPaneExecShell{Command: q.Command})
		return coord.HostResponse{Err: e}
	}
	return coord.HostResponse{Err: fmt.Errorf("unknown local peer host operation")}
}
func (w *WorkspaceActor) coordinationCommand(m *msg.MsgCLIPeerCommand, reply func(interface{}) error) {
	runtime, workspace, pane := w.peerRuntime, w.workspaceName, m.AsPaneID
	// This path never borrows focus. It carries the caller's pane to the runtime,
	// which binds managed task panes to their registered endpoint and attempt.
	args := append([]string{}, m.Args...)
	go func() {
		response := &msg.MsgCLIResponse{}
		if runtime == nil {
			response.Error = "peer listener unavailable"
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 24*time.Second)
			defer cancel()
			result, e := runtime.Command(ctx, workspace, pane, args)
			if e != nil {
				response.Error = e.Error()
			} else {
				response.OK = true
				if text, ok := result.(string); ok {
					response.Output = text
				} else {
					data, e := json.Marshal(result)
					if e != nil {
						response.OK = false
						response.Error = e.Error()
					} else {
						response.Output = string(data) + "\n"
					}
				}
			}
		}
		_ = reply(response)
	}()
}
