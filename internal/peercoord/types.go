// SPDX-License-Identifier: Apache-2.0

// Package peercoord runs session-local peer listeners. Only typed messages and
// task records cross the server boundary; Host operations are strictly local.
package peercoord

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

type HostRequest struct {
	Args                                                                     []string
	Workspace, Op, Origin, Pane, Execution, Directory, Command, Name, Secret string
}
type HostResponse struct {
	Value  string
	Exists bool
	Err    error
}
type Host func(context.Context, HostRequest) HostResponse

type Project struct {
	Path string `json:"path"`
	Base string `json:"base"`
}
type Endpoint struct {
	LastProbe       time.Time `json:"-"`
	SetupError      string    `json:"setup_error,omitempty"`
	Enrolled        bool      `json:"enrolled"`
	Agent           pc.Agent  `json:"agent"`
	Origin          string    `json:"origin"`
	Mode            string    `json:"mode"`
	AcceptedLeaders []string  `json:"accepted_leaders"`
	Permission      string    `json:"permission"`
	Model           string    `json:"model,omitempty"`
	MaxRuntime      int       `json:"max_runtime"`
	MaxTurns        int       `json:"max_turns"`
	MaxBudgetUSD    float64   `json:"max_budget_usd"`
	LeaderExecution string    `json:"leader_execution,omitempty"`
}
type Outgoing struct {
	Request pc.Request `json:"request"`
	Token   string     `json:"token,omitempty"`
}
type Execution struct {
	SpentUSD       float64    `json:"spent_usd"`
	CommandToken   string     `json:"command_token"`
	LastDeliveryID string     `json:"last_delivery_id,omitempty"`
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	TaskID         string     `json:"task_id,omitempty"`
	AttemptID      string     `json:"attempt_id,omitempty"`
	Token          string     `json:"token"`
	PaneID         string     `json:"pane_id"`
	Directory      string     `json:"directory"`
	Branch         string     `json:"branch"`
	Base           string     `json:"base"`
	Phase          string     `json:"phase"`
	CreatedAt      time.Time  `json:"created_at"`
	Deadline       time.Time  `json:"deadline"`
	LastSeen       time.Time  `json:"last_seen"`
	Incarnation    string     `json:"incarnation"`
	Conversation   string     `json:"conversation,omitempty"`
	Inbox          []pc.Event `json:"inbox"`
	Delivery       []pc.Event `json:"delivery"`
	DeliveryID     string     `json:"delivery_id,omitempty"`
	Turns          int        `json:"turns"`
	Paused         bool       `json:"paused"`
	Error          string     `json:"error,omitempty"`
	InitialPrompt  string     `json:"initial_prompt"`
}
type Board struct {
	Recent          []pc.Event            `json:"recent"`
	AdmissionErrors map[string]string     `json:"admission_errors,omitempty"`
	Connection      peer.Connection       `json:"connection"`
	Workspace       string                `json:"local_workspace"`
	PeerID          string                `json:"peer_id"`
	Token           string                `json:"token"`
	Incarnation     string                `json:"incarnation"`
	Listening       bool                  `json:"listening"`
	Draining        bool                  `json:"draining"`
	Cursor          int64                 `json:"cursor"`
	Projects        map[string]Project    `json:"projects"`
	Endpoints       map[string]*Endpoint  `json:"endpoints"`
	Executions      map[string]*Execution `json:"executions"`
	Inbox           []pc.Event            `json:"inbox"`
	Outbox          []Outgoing            `json:"outbox"`
	State           pc.State              `json:"state"`
	LastContact     time.Time             `json:"last_contact"`
	LastError       string                `json:"last_error,omitempty"`
	Rejoined        bool                  `json:"-"`
}
type Disk struct {
	Version int               `json:"version"`
	Boards  map[string]*Board `json:"boards"`
}
type Runtime struct {
	connected                               atomic.Bool
	probe                                   func(context.Context, string) error
	mu                                      sync.Mutex
	root, session, dir, socket, incarnation string
	host                                    Host
	data                                    Disk
	stop                                    chan struct{}
	done                                    chan struct{}
	closeServer                             func() error
	lock                                    *os.File
	fatal                                   error
}

func Directory(root, session string) string {
	return filepath.Join(root, "peer-coordination", fmt.Sprintf("%x", sha256.Sum256([]byte(session)))[:24])
}
func BoardKey(c peer.Connection) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(c.URL+"\n"+c.Workspace+"\n"+c.Board)))
}
func WriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".peer-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (r *Runtime) save() error {
	if r.fatal != nil {
		return r.fatal
	}
	err := WriteJSON(filepath.Join(r.dir, "state.json"), r.data)
	if err != nil {
		r.fatal = fmt.Errorf("peer persistence failed; admission stopped: %w", err)
	}
	return err
}
func IsCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "init", "join", "leave", "register", "members", "agents", "state", "summary", "events", "project", "listen", "enroll", "leader", "task", "send", "board", "local", "help":
		return true
	}
	return false
}
