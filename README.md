# rysh-cli-code

The [Rysh](https://github.com/rysh-ai/rysh-cli-parent) CLI: an agentic terminal
multiplexer written in Go. Tabs, panes, splits, and `vim`/`htop` working exactly
as you expect — except every pane is also an agent that can answer prompts and
call tools.

This repository holds the CLI itself. It depends on
[`rysh-cli-shared`](https://github.com/rysh-ai/rysh-cli-shared).

**Building or contributing? Start at
[`rysh-cli-parent`](https://github.com/rysh-ai/rysh-cli-parent)** — it carries the
Makefile, the Go workspace, and CI, and wires this module to a local checkout of
the shared one.

## Video tutorials

Five narrated, subtitled walkthroughs of what rysh actually does, in order —
**each one assumes the previous**. Every player below is live on this page; press
play, nothing leaves GitHub. The first four are recorded against the build the
[install](#install) line gives you, so every command in them is one you can type.

<table>
<tr>
<td width="50%">
<b>1 — Install, and your first tabs, lanes and stacked panes</b> · 1:29

<video src="https://github.com/user-attachments/assets/bbb99804-2516-46c5-aede-4d5bb0668aa1" controls muted></video>

<code>go install</code>, a first session, a tab, a second lane, a stack of three panes — then <code>##claude</code> in each pane of that stack, so one window holds three independent Claude sessions.
</td>
<td width="50%">
<b>2 — Stop the session, start it again — the agents are still there</b> · 2:44

<video src="https://github.com/user-attachments/assets/41dc48e5-5c63-4185-ba6a-19cb6a96783e" controls muted></video>

Why a session is a daemon and not a terminal. Claude and Codex are each told a codeword, the session is stopped <i>outright</i>, and after the restart both resume unprompted and answer with the codeword they were told before it.
</td>
</tr>
<tr>
<td width="50%">
<b>3 — Claude and Codex in three stacked panes at once</b> · 2:07

<video src="https://github.com/user-attachments/assets/2939ab4f-35c6-4a92-af4a-8564fdab4f23" controls muted></video>

More than one agent, and more than one vendor, side by side: <code>Ctrl+S</code> and a digit to move around a stack, a task each, three files written in parallel.
</td>
<td width="50%">
<b>4 — A fleet that talks to itself, on a shared board</b> · 2:29

<video src="https://github.com/user-attachments/assets/6523e041-90e1-422e-9992-422d46180020" controls muted></video>

<code>##board open</code>, <code>rysh ansa prompt</code>, and agents driving <i>each other</i>: roadmap (Claude) sets the goal, fleet-manager (Codex) splits it into work orders, two workers build in parallel, and the manager sends a correction back to a worker before signing off. They ship a browser todo manager — no backend, no build step.
</td>
</tr>
<tr>
<td width="50%" colspan="2">
<b>5 — Graph engineering — designing the shape of a fleet</b> · 4:00

<video src="https://github.com/user-attachments/assets/5d42927b-f6c6-4149-9a88-77742163da3c" controls muted></video>

The deep end: why an agent org chart is a graph, what the edges mean, and what changes when you remove one. Orders travel <i>down</i> as messages, results come <i>up</i> on the board, and two workers with no edge between them give independent verdicts.
</td>
</tr>
</table>

Start at 1 even if you have used tmux or Zellij for years — the panes look
familiar and the session model underneath them does not, which is video 2.

## Secrets stay on your machine

SecretNAT is on by default. Secrets are substituted with tokens in the request body
before it leaves the machine, and a response carrying a live credential in plaintext
is reported into the pane. Responses are not rewritten — by design.

The mapping is reversible locally, so `##snat get <token>` hands you back the real
value in your own pane, and the model only ever saw the token.

## Install

Two binaries exist and they are **not interchangeable**:

| | this repository | the prebuilt distribution |
| --- | --- | --- |
| binary name | `rysh` | `ry` |
| how you get it | `go install`, or the installer below | its own channels, not documented here |

The names differ so that one machine can carry both. **Every install command in
this README installs `rysh`**, and a script, alias or doc written for one does
not run against the other — which is the whole reason the names differ. Check
what you have with `rysh version` or `ry version`.

This repository is the `rysh` half: Apache-2.0, with [LICENSE](LICENSE) and
[NOTICE](NOTICE) beside the source you are reading.

### macOS and Linux

**The open-source build, from source:**

```sh
go install github.com/rysh-ai/rysh-cli-code/cmd/rysh@latest
```

The binary lands in `$(go env GOPATH)/bin` as `rysh`. Requires **Go 1.25.3 or
newer** — the floor declared in this module's `go.mod`.

**The open-source build, prebuilt:**

```sh
curl -fsSL https://packages.rysh.ai/install-rysh.sh | sh
```

Installs `rysh` from this repository's own releases — the Apache-2.0 binary, not
`ry`. It resolves the newest release at run time; set `RYSH_VERSION` to pin one
instead, or `RYSH_INSTALL_DIR` to choose where it lands.

**The prebuilt distribution — installs `ry`, not `rysh`.** It is the packaged
product and a separate distribution; nothing in it is produced by this repository,
so `ry --help` describing a command is not evidence about the source here. Use
`go install` above if you want the build whose source you are reading.

Its install commands are deliberately not reproduced here: this README documents
the open-source build. The prebuilt distribution has its own channels and its own
documentation.

### Windows

**WSL2 is the supported path.** Native Windows compiles and runs, but it cannot
open a pane — those are two different claims and both are true:

- `GOOS=windows GOARCH=amd64 go build ./...` succeeds — the windows/amd64 target
  builds clean from this source. This repository's `.goreleaser.yml` names that
  archive `cli-only` on purpose, so the artifact says what it is.
- It **cannot start a session.** Rysh's PTY layer has no ConPTY implementation, so
  `platform.PTYSupported` is `false` on Windows and every session-opening command
  is refused up front with WSL guidance — rather than starting a session and then
  failing on your first pane (`cmd/rysh/pty_preflight.go`).
- What it *can* do is the pane-less command set, including talking to a session
  running in WSL from the Windows side: `rysh send`, `rysh exec`, `rysh prompt`,
  `rysh list-sessions`, `rysh install`, `rysh eval`, `rysh doctor`.

For a real session, install WSL2 and use the Linux instructions inside it — the
Linux build runs unmodified:

```powershell
wsl --install
```

```sh
# inside WSL
go install github.com/rysh-ai/rysh-cli-code/cmd/rysh@latest   # rysh
```

Native panes need ConPTY behind the same seam; it is not implemented yet.

### Build from scratch

This module alone builds standalone — it carries no `replace` directive, so the
shared module resolves from its published version:

```sh
git clone https://github.com/rysh-ai/rysh-cli-code
cd rysh-cli-code
go build ./cmd/rysh          # -> ./rysh
```

From the superrepo, which is how to develop against both modules at once — its
`go.work` wires the shared module to the sibling checkout, so an edit there is
picked up without a release round-trip:

```sh
git clone --recursive https://github.com/rysh-ai/rysh-cli-parent
cd rysh-cli-parent
make build       # -> bin/rysh
make install     # -> ~/.local/bin/rysh   (override with PREFIX=)
make test        # both modules
```

Go 1.25.3 or newer either way.

## First run

```sh
export ANTHROPIC_API_KEY=sk-ant-...
rysh onboard --provider anthropic --key-env ANTHROPIC_API_KEY
rysh doctor
```

`onboard` validates the key, writes a project-local `rysh.config.yaml`, and opens
your session. `rysh --help` lists the full command surface.

Each pane has an input mode and `Esc Esc` cycles it: **shell** (a real PTY) →
**prompt** (goes to the LLM) → **rysh** (multiplexer commands) → **chat**
(conversation, no tools). Those four are enabled by default; `##mode list` shows
what a pane has and `##mode new <mode>` adds the rest.

The [parent repo README](https://github.com/rysh-ai/rysh-cli-parent#readme) has
the keybindings, sessions, agents and humanoids.

## A first session, start to finish

One path from nothing to two Claude panes posting to a board. Step 4 needs
[Claude Code](https://docs.anthropic.com/en/docs/claude-code) installed as
`claude`. `##codex` works the same way if you use Codex instead.

**1. Install and onboard**, in the directory you want to work in. Config is
project-local, so onboarding writes `rysh.config.yaml` and `.rysh/` right here:

```sh
go install github.com/rysh-ai/rysh-cli-code/cmd/rysh@latest
export ANTHROPIC_API_KEY=sk-ant-...
rysh onboard --provider anthropic --key-env ANTHROPIC_API_KEY
```

**2. Open a session.**

```sh
rysh create first
```

You are now in one pane, in shell mode. Press `Esc Esc` until the pane's mode
reads **rysh**.

**3. Make a stack.**

```
##new stack 2
```

The pane you typed in now has two more under it, in one stack. `Ctrl+S` then a
digit jumps to that pane of the stack, and `Esc` leaves stack mode.

**4. Put Claude in two of them.** In pane 2 and then pane 3, in rysh mode:

```
##claude
```

Each pane now runs its own interactive Claude session. Give each a different
task.

**5. Open the board** from pane 1:

```
##board open
```

Ask either Claude to report on the board when it finishes. From inside its
pane, a post is one shell command, and `rysh board tail` reads the stream back:

```sh
rysh board post --as "$RYSH_PANE" -- 'parser wiring done'
rysh board tail --limit 20
```

The board belongs to this session, on this machine.

**6. Step away.** `Ctrl+P d` detaches and the session keeps running;
`rysh attach first` brings you back. The tours below go deeper into each step.

## Three tours

Commands starting with `##` are typed into a pane in **rysh** mode. From outside
the session, `rysh exec -- '##<cmd>'` runs the same thing and prints its output.

### 1. Create a session

```sh
rysh create work          # create and attach; add -d to leave it detached
rysh list-sessions        # what is running
rysh attach work          # come back to it later
rysh detach work          # leave it running
rysh stop work            # shut the daemon down
```

A session is a daemon plus its panes. It outlives your terminal, so closing the
window detaches rather than kills.

### 2. Tabs, lanes, panes and stacks

A **tab** holds **lanes** (columns); a lane holds **stacks**; a stack holds panes,
Zellij-style — one expanded, the rest collapsed to a title bar showing `[N/M]`.

```
##new tab                 a new tab
##new lane                a new lane (column) in the active tab
##new pane                a pane at the bottom of the active lane
##new grid 4              4 panes stacked in the active lane
##new grid 3x4            3 lanes x 4 panes, in the active tab
##new grid 2x3x4          2 tabs x 3 lanes x 4 panes
##new stack 4             4 more stacked panes in the active stack
```

Look at what you built, and move things around:

```
##tab list                ##lane list           ##pane list
##panegroup layout        the lane layout, whole
##move pane up|down|left|right      reorder in the stack, or cross to the next lane
##move pane <p> to-lane <lane>      put a live pane somewhere else
##tab name build          ##lane name left      ##pane name builder
```

A stack with an agent in every pane is video 3 of the
[tutorials](#video-tutorials) at the top of this page.

### 3. The agents board, fleets, and a fleet's own board

Panes can run an interactive **Claude** or **Codex**, and rysh remembers which, so
a session stop/start resumes the same conversation:

```
##claude                  run claude in this pane, resumed automatically
##codex                   run codex in this pane, same deal
##pane new --claude "start on the parser"    a new pane, already working
```

The **agents board** is where those panes talk to each other and to you:

```
##board open              open the board pane
##board post <text>       post a milestone
##board reply <thread> <text>
```

An agent inside a pane posts without stealing focus, and reads back:

```sh
rysh board post --as "$RYSH_PANE" -- 'parser wiring done'
rysh board tail --limit 20
```

`##board agent up` starts a **board agent** — a hidden pane running Claude that
sits in the path, routes messages between agents, and refuses a request it knows
is stale. `##board agent visible` draws it without moving your focus;
`##board agent invisible` puts it away while it keeps running.

A **fleet** is a named group of panes with a board of its own. Registering one
opens no panes — it is the cheap half:

```
##fleet register epic-07 --board epic-07
##fleet state epic-07 up
##fleet list              ##fleet show epic-07
##fleet forget epic-07    drop it; the panes keep running
```

```sh
rysh board tail --fleet epic-07     # that fleet's stream, not the session's
```

A fleet may be claudes, codexes, or both — the commands are the same either way.

A fleet of Claude and Codex panes working off one shared board is video 4 of the
[tutorials](#video-tutorials) at the top of this page; how to choose a fleet's
shape — who sends orders to whom, and who reports where — is video 5.

## Examples

`examples/` in this repository is runnable. None of it needs a provider key.

**Scripts.** A `.rysh` file is a bash script whose `##` lines are rysh commands:
`rysh script` runs them, and plain `bash` reads them as comments. A script
refuses to drive a session somebody is attached to, so point it at a detached
one:

```sh
rysh create demo -d
rysh script examples/scripts/deploy-check.rysh --session demo staging
```

| Script | What it shows |
| --- | --- |
| `deploy-check.rysh` | the format itself: a `##` line's output in `$RYSH_OUT`, its exit status in `$?`, and a command that is expected to fail |
| `workspace-setup.rysh` | a tab and a grid, with each pane named after a role — `rysh script … --session demo 4` for four panes |
| `morning-standup.rysh` | one pane per repository given as an argument, named after it, with `git log` fanned out across them |
| `fanout.rysh` | one command run in every pane of a lane, tab or workspace — `rysh script … --session demo tab 'git status -s'` |
| `bootstrap-env.rysh` | seeding `##variable`s (the agent can read them) and `##secret`s (it cannot) from your environment, safely re-runnable |
| `ci-gate.rysh` | a CI gate that boots its own throwaway session — run it with `--ephemeral` in place of `--session` |

Each script's header comment has the full usage.

**A fleet on its own board.** `examples/fleet/fleet-board.rysh` registers a
fleet, stacks three member panes (a lead and two workers), and has each one
post to the fleet's board from its own pane. Then it reads the board back.
Run it in a fresh session:

```sh
rysh create fleet-demo -d
rysh script examples/fleet/fleet-board.rysh --session fleet-demo
rysh board tail --fleet demo-fleet --session fleet-demo
```

The panes run plain shells. The comment at the end of the script is the next
step: `##claude` in each member, with work handed out by `rysh ansa prompt` and
reported back on the board: a smaller version of what video 4 shows, on one machine.

**A channel plugin.** `examples/plugins/echo-channel` is the reference stdio
channel plugin. It uses only the standard library, and every message sent to it
comes straight back:

```sh
(cd examples/plugins/echo-channel && go build -o echo-channel .)
rysh channel install examples/plugins/echo-channel
rysh channel list
```

`go test ./examples/plugins/echo-channel/` runs its test.

## Layout

| Path | What |
| --- | --- |
| `cmd/rysh` | the main package — entry point and command surface |
| `internal/tui` | the terminal UI |
| `internal/actors` | workspace / tab / pane / agent actors, proto.actor over NATS |
| `internal/vterm` | terminal emulation, including a vt10x fork with scrollback |
| `internal/provider` | LLM provider adapters |
| `internal/platform` | host capabilities that change what rysh can do (e.g. PTY support) |
| `action/` | the `setup-rysh` composite GitHub Action |

## Contributing and security

**This repository is a one-way export** of a tree developed elsewhere, so a commit
pushed straight here is overwritten by the next export rather than kept. That does
not make patches pointless — [CONTRIBUTING.md](CONTRIBUTING.md) explains where one
actually lands.

**Found a vulnerability? Do not open a public issue, PR or discussion.** Rysh runs
shell commands and holds provider credentials on a developer's machine, so a public
report is an exploit notice. [SECURITY.md](SECURITY.md) has the private channels.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
