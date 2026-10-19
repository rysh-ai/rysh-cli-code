// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"fmt"
	"strconv"
	"strings"
)

const Usage = `Peer coordination (messages only)
  peer connect URL --key-secret NAME --board BOARD
  peer init --name leader --runner codex
  peer join --name worker --runner claude --project app
  peer register --name leader --role leader --runner claude --project app
  peer leave
  peer project add app /absolute/repository [--base main]
  peer enroll worker --accept-leader SESSION/leader --permission workspace --mode auto
  peer listen start|pause|drain|stop|status
  peer state|members|agents|events [--json]
  peer summary --json                  # counts and human attention queue
  peer leader objective -- OBJECTIVE
  peer leader restart [--confirm-stopped]
  peer leader handover SESSION/leader
  peer leader standbys --agents ID,ID [--auto]
  peer send --to SESSION/agent -- TEXT
  peer task assign --to SESSION/worker --project app --criteria TEXT [--depends T-1,T-2] [--max-runtime SECONDS] -- OBJECTIVE
  peer task list
  peer task show|open|admit TASK
  peer task progress TASK [--state blocked] -- TEXT
  peer task result TASK -- SUMMARY
  peer task checkpoint TASK -- MESSAGE  # commit this task's local worktree
  peer task review TASK --accept -- TEXT
  peer task cancel|retry|resolve TASK [--to AGENT] [--confirm-stopped] -- REASON
  peer board open|watch|show
  peer local pause|resume EXECUTION
Options: --board NAME, --agent NAME, --id UUID, --json.
Run commands from the session directory or with RYSH_DIR set.
`

type Arguments struct {
	Verb, Sub string
	Pos       []string
	Flags     map[string]string
	Text      string
}

func Parse(args []string) (Arguments, error) {
	a := Arguments{Flags: map[string]string{}}
	if len(args) == 0 {
		return a, fmt.Errorf("%s", Usage)
	}
	a.Verb = args[0]
	bools := map[string]bool{"json": true, "accept": true, "auto": true, "confirm-stopped": true}
	values := map[string]bool{"board": true, "agent": true, "id": true, "name": true, "role": true, "runner": true, "project": true, "capabilities": true, "max-active": true, "base": true, "mode": true, "permission": true, "model": true, "max-runtime": true, "max-turns": true, "max-budget-usd": true, "accept-leader": true, "to": true, "criteria": true, "depends": true, "state": true, "agents": true, "after": true, "result-json": true, "task": true}
	for i := 1; i < len(args); i++ {
		v := args[i]
		if v == "--" {
			a.Text = strings.Join(args[i+1:], " ")
			break
		}
		if strings.HasPrefix(v, "--") {
			k := strings.TrimPrefix(v, "--")
			if _, exists := a.Flags[k]; exists {
				return a, fmt.Errorf("duplicate --%s", k)
			}
			if bools[k] {
				a.Flags[k] = "true"
				continue
			}
			if !values[k] || i+1 >= len(args) {
				return a, fmt.Errorf("unknown or missing option %s", v)
			}
			i++
			a.Flags[k] = args[i]
			continue
		}
		a.Pos = append(a.Pos, v)
	}
	switch a.Verb {
	case "project", "listen", "leader", "task", "board", "local":
		if len(a.Pos) == 0 {
			return a, fmt.Errorf("%s needs a subcommand", a.Verb)
		}
		a.Sub = a.Pos[0]
		a.Pos = a.Pos[1:]
	}
	if b := a.Flags["board"]; b != "" && !validName(b) {
		return a, fmt.Errorf("invalid board")
	}
	return a, nil
}
func (a Arguments) int(k string, def int) (int, error) {
	if a.Flags[k] == "" {
		return def, nil
	}
	n, e := strconv.Atoi(a.Flags[k])
	if e != nil {
		return 0, fmt.Errorf("invalid --%s", k)
	}
	return n, nil
}
func (a Arguments) one() (string, error) {
	if len(a.Pos) != 1 {
		return "", fmt.Errorf("%s %s needs one argument", a.Verb, a.Sub)
	}
	return a.Pos[0], nil
}
func csv(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}
