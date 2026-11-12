---
name: use-rysh
description: Drive rysh from a coding agent (Claude Code, Codex, ...) — start sandboxed agent panes, read the fleet board, and hand every approval to a person.
---

You drive a rysh session from outside it, with the `rysh` command line. A
**session** is a daemon on one machine; it holds tabs, lanes and **panes**, and
each pane is a shell or an agent (claude, codex, or rysh's own). A person may be
watching the same panes in their rysh window, so act as a careful colleague in a
shared room.

## 1. Find the session and keep your target explicit

```sh
rysh list-sessions                      # which sessions exist and which are alive
rysh create work -d                     # start one detached, only if the person asked
rysh exec --session work -- '##pane list'
```

- Always pass `--session <name>`, and `--pane-id <id>` for anything aimed at a
  pane. Without them a command follows whatever pane the person has in focus.
- `rysh exec --session <s> [--pane-id <p>] [--json] -- '##<command>'` runs any
  `##` command and exits with its status; `--json` gives one result line.
- Never stop or delete a session you did not create (`rysh stop`,
  `rysh delete-session`) unless the person asks.

## 2. Start an agent in a sandboxed pane

```sh
rysh exec --session work -- '##new pane'
rysh exec --session work -- '##pane list'          # find the new pane's id
rysh exec --session work --pane-id <p> -- '##claude --sandbox default --permission-mode manual'
rysh exec --session work --pane-id <p> -- '##codex --sandbox default -a on-request'
```

- `--sandbox <profile>` runs the agent inside a sandbox: on macOS under
  `sandbox-exec`, on Linux in `bwrap` or a container. It **fails closed**: if
  the sandbox cannot be set up, the agent does not start. Report that to the
  person; never retry without `--sandbox`.
- Profiles live in `.rysh/sandbox.yaml`; `network: off` gives the agent no
  network. With no file, only the built-in `default` profile exists.
- The pane shows `[sbx:<profile>]` in its title and `sandbox=<profile>` in
  `##pane list`. Check it before you hand the agent work.
- `--sandbox` is not supported yet with `##pane new --claude`, with
  `--fresh-session` / `--resume-dir`, in fleet panes or in remote panes.
- By default rysh launches a claude or codex pane with its approval prompts
  turned off: the pane (and the sandbox) is the boundary. When the person wants
  to approve risky steps, pass `--permission-mode manual` (claude) or
  `-a on-request` (codex), as above.

## 3. Give it work, then wait

```sh
rysh ansa send <p> -- 'run the tests and fix the first failure'
rysh wait --session work --pane-id <p> --until idle --timeout 30m   # 0 reached, 5 timeout
rysh screen --session work --pane-id <p>                            # what it shows now
```

`rysh send --mode prompt` into a claude pane is refused; use `rysh ansa send`.
For a rysh-native agent pane, `rysh prompt --session <s> --pane-id <p> -- '<text>'`
sends the prompt and blocks until the turn ends.

## 4. Approvals belong to a person

```sh
rysh wait --session work --pane-id <p> --until needs-you    # a permission prompt is up
rysh screen --session work --pane-id <p>                    # read what it asks
```

- **Never approve, deny or dismiss a permission prompt yourself** — not with
  `rysh ansa keys`, not with `rysh send`, not by any other route. Tell the
  person which session and pane need them and what the prompt asks, then wait.
- `rysh wait` is read-only: it never answers a prompt or approves a gate.
- Policy gates fail closed. `rysh prompt` exits **3** when a gate blocked the
  turn: treat it as "a person must decide", never as an error to route around.
- Never relaunch an agent with its approvals turned off to get past a prompt.

## 5. Many agents: the boards

```sh
rysh fleet board                        # one row per fleet: active, quiet, blocked or down
rysh board tail --limit 20              # recent threads on the agents board
rysh board post -- 'tests green on the parser branch'
```

On screen, `##board open --board fleet` shows the fleet board read-only. A
fleet that reads `blocked` usually means an agent needs a person: check it with
`rysh wait --until needs-you` and tell the person.

## 6. Undo an agent's file changes

`rysh undo --session <s> --pane-id <p> --list` lists the checkpoints of a pane;
restoring one changes the working tree, so ask the person first.

## Rules

- Secrets by name only (`##secret`); never echo, log or paste a key.
- Ask before anything you cannot undo: stopping sessions or restoring
  checkpoints.
- When in doubt, read (`##pane list`, `rysh screen`, `rysh fleet board`)
  rather than act.
